package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	pageSize   = 60
	presignTTL = time.Hour
	// Redirects to presigned URLs are cached a bit less than the URL lives.
	redirectMaxAge = 50 * time.Minute
)

// allowedTypes maps accepted image MIME types to the extension used in S3.
var allowedTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/heic": ".heic",
	"image/avif": ".avif",
}

type App struct {
	cfg   *Config
	db    *DB
	store Store
	oauth *oauth2.Config
	tmpl  map[string]*template.Template
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

	mux.HandleFunc("GET /login", a.handleLoginPage)
	mux.HandleFunc("GET /auth/google", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("POST /logout", a.sameOrigin(a.handleLogout))

	mux.HandleFunc("GET /{$}", requireUser(a.handleGallery))
	mux.HandleFunc("POST /api/photos", requireUser(a.sameOrigin(a.handleUpload)))
	mux.HandleFunc("GET /p/{id}", requireUser(a.handlePhotoPage))
	mux.HandleFunc("GET /p/{id}/original", requireUser(a.handlePhotoRedirect(false)))
	mux.HandleFunc("GET /p/{id}/thumb", requireUser(a.handlePhotoRedirect(true)))
	mux.HandleFunc("POST /p/{id}/delete", requireUser(a.sameOrigin(a.handleDelete)))

	return securityHeaders(a.withUser(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func loadTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"humanBytes": humanBytes,
		"daysLeft": func(t time.Time) int {
			return int(math.Ceil(time.Until(t).Hours() / 24))
		},
	}
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, p := range pages {
		name := path.Base(p)
		if name == "layout.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", p)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

func (a *App) render(w http.ResponseWriter, name string, data map[string]any) {
	var buf bytes.Buffer
	if err := a.tmpl[name].Execute(&buf, data); err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (a *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if userFrom(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	a.render(w, "login.html", nil)
}

func (a *App) handleGallery(w http.ResponseWriter, r *http.Request) {
	var beforeTime time.Time
	var beforeID string
	if c := r.URL.Query().Get("before"); c != "" {
		ts, id, ok := strings.Cut(c, "_")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if !ok || err != nil {
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		beforeTime, beforeID = t, id
	}
	photos, err := a.db.ListPhotos(r.Context(), beforeTime, beforeID, pageSize+1)
	if err != nil {
		a.serverError(w, err)
		return
	}
	next := ""
	if len(photos) > pageSize {
		photos = photos[:pageSize]
		last := photos[len(photos)-1]
		next = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "_" + last.ID
	}
	a.render(w, "gallery.html", map[string]any{
		"User":     userFrom(r.Context()),
		"Photos":   photos,
		"Next":     next,
		"View":     "grid", // the only view for now
		"MaxBytes": maxUploadBytes,
		"TTLDays":  int(photoTTL / (24 * time.Hour)),
		"Accept":   acceptAttr(),
	})
}

func (a *App) handlePhotoPage(w http.ResponseWriter, r *http.Request) {
	p, err := a.db.GetPhoto(r.Context(), r.PathValue("id"))
	if err != nil {
		a.serverError(w, err)
		return
	}
	if p == nil {
		http.NotFound(w, r)
		return
	}
	u := userFrom(r.Context())
	a.render(w, "photo.html", map[string]any{"User": u, "Photo": p, "CanDelete": p.UserID == u.ID})
}

func (a *App) handlePhotoRedirect(thumb bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.db.GetPhoto(r.Context(), r.PathValue("id"))
		if err != nil {
			a.serverError(w, err)
			return
		}
		if p == nil {
			http.NotFound(w, r)
			return
		}
		key := p.ObjectKey
		if thumb && p.ThumbKey != "" {
			key = p.ThumbKey
		}
		u, err := a.store.PresignGet(r.Context(), key, presignTTL)
		if err != nil {
			a.serverError(w, err)
			return
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", int(redirectMaxAge.Seconds())))
		http.Redirect(w, r, u, http.StatusFound)
	}
}

func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, err := a.db.GetPhoto(r.Context(), r.PathValue("id"))
	if err != nil {
		a.serverError(w, err)
		return
	}
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if p.UserID != userFrom(r.Context()).ID {
		http.Error(w, "you can only delete your own photos", http.StatusForbidden)
		return
	}
	if err := a.store.Delete(r.Context(), p.ObjectKey, p.ThumbKey); err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.db.DeletePhoto(r.Context(), p.ID); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type uploadError struct {
	status int
	msg    string
}

func (e *uploadError) Error() string { return e.msg }

// handleUpload accepts one photo per request as multipart/form-data with a
// "file" part (<= 10 MiB) and an optional browser-generated "thumb" part.
// The browser sends multi-selections as parallel requests, so one bad file
// does not fail the whole batch.
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+maxThumbBytes+64<<10)
	file, filename, thumb, err := readUpload(r)
	if err != nil {
		var ue *uploadError
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &ue):
			jsonError(w, ue.status, ue.msg)
		case errors.As(err, &mbe):
			jsonError(w, http.StatusRequestEntityTooLarge, "file is larger than 10 MB")
		default:
			jsonError(w, http.StatusBadRequest, "could not read upload")
		}
		return
	}

	ctype, ok := sniffImage(file)
	if !ok {
		jsonError(w, http.StatusUnsupportedMediaType, "not a supported image type (JPEG, PNG, GIF, WebP, HEIC, AVIF)")
		return
	}

	id := newID()
	now := time.Now().UTC()
	p := &Photo{
		ID:          id,
		UserID:      userFrom(r.Context()).ID,
		ObjectKey:   a.key("photos", id+allowedTypes[ctype]),
		Filename:    filename,
		ContentType: ctype,
		SizeBytes:   int64(len(file)),
		CreatedAt:   now,
		ExpiresAt:   now.Add(photoTTL),
	}
	if err := a.store.Put(r.Context(), p.ObjectKey, ctype, file); err != nil {
		a.serverError(w, err)
		return
	}
	if len(thumb) > 0 {
		if tt, ok := sniffImage(thumb); ok && (tt == "image/jpeg" || tt == "image/webp") {
			key := a.key("thumbs", id+allowedTypes[tt])
			if err := a.store.Put(r.Context(), key, tt, thumb); err != nil {
				log.Printf("thumbnail upload for %s failed, using original: %v", id, err)
			} else {
				p.ThumbKey = key
			}
		}
	}
	if err := a.db.InsertPhoto(r.Context(), p); err != nil {
		_ = a.store.Delete(r.Context(), p.ObjectKey, p.ThumbKey)
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func readUpload(r *http.Request) (file []byte, filename string, thumb []byte, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", nil, &uploadError{http.StatusBadRequest, "expected multipart/form-data"}
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", nil, err
		}
		switch part.FormName() {
		case "file":
			if file, err = readLimited(part, maxUploadBytes); err != nil {
				return nil, "", nil, err
			}
			filename = cleanFilename(part.FileName())
		case "thumb":
			if thumb, err = readLimited(part, maxThumbBytes); err != nil {
				return nil, "", nil, err
			}
		}
		part.Close()
	}
	if len(file) == 0 {
		return nil, "", nil, &uploadError{http.StatusBadRequest, "no file in upload"}
	}
	return file, filename, thumb, nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, &uploadError{http.StatusRequestEntityTooLarge, fmt.Sprintf("file is larger than %s", humanBytes(limit))}
	}
	return b, nil
}

// sniffImage identifies the image type from magic bytes, ignoring whatever
// Content-Type the client claimed.
func sniffImage(b []byte) (string, bool) {
	ct := http.DetectContentType(b)
	if _, ok := allowedTypes[ct]; ok && ct != "image/heic" && ct != "image/avif" {
		return ct, true
	}
	// ISO-BMFF images: bytes 4..8 are "ftyp" followed by a major brand.
	if len(b) >= 12 && string(b[4:8]) == "ftyp" {
		switch string(b[8:12]) {
		case "heic", "heix", "heim", "heis", "mif1", "msf1":
			return "image/heic", true
		case "avif", "avis":
			return "image/avif", true
		}
	}
	return "", false
}

func cleanFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || !utf8.ValidString(name) {
		return "photo"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func (a *App) key(kind, name string) string {
	if a.cfg.S3Prefix == "" {
		return kind + "/" + name
	}
	return a.cfg.S3Prefix + "/" + kind + "/" + name
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func acceptAttr() string {
	exts := []string{}
	for ct, ext := range allowedTypes {
		exts = append(exts, ct, ext)
	}
	return strings.Join(append(exts, ".jpeg", ".heif"), ",")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
