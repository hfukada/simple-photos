package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSniffImage(t *testing.T) {
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00")...)
	cases := []struct {
		in   []byte
		want string
		ok   bool
	}{
		{testJPEG(t), "image/jpeg", true},
		{[]byte("\x89PNG\r\n\x1a\n0000"), "image/png", true},
		{heic, "image/heic", true},
		{[]byte("<html><script>alert(1)</script>"), "", false},
		{[]byte("plain text"), "", false},
	}
	for _, c := range cases {
		got, ok := sniffImage(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("sniffImage(%q...) = %q, %v; want %q, %v", c.in[:8], got, ok, c.want, c.ok)
		}
	}
}

func TestCleanFilename(t *testing.T) {
	for in, want := range map[string]string{
		"IMG_1.jpg":             "IMG_1.jpg",
		"../../etc/passwd":      "passwd",
		`C:\Users\me\photo.png`: "photo.png",
		"":                      "photo",
		"bad\xffutf8.jpg":       "photo",
	} {
		if got := cleanFilename(in); got != want {
			t.Errorf("cleanFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailAllowed(t *testing.T) {
	c := &Config{AllowedEmails: csvSet("a@x.com"), AllowedDomains: csvSet("family.org")}
	for email, want := range map[string]bool{
		"a@x.com": true, "A@X.com": true, "b@x.com": false,
		"kid@family.org": true, "evil@notfamily.org": false,
	} {
		if got := c.emailAllowed(email); got != want {
			t.Errorf("emailAllowed(%q) = %v", email, got)
		}
	}
	if !(&Config{}).emailAllowed("anyone@gmail.com") {
		t.Error("empty allowlist should allow everyone")
	}
}

// --- integration tests (need TEST_DATABASE_URL) ---

type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memStore) Put(_ context.Context, key, _ string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = body
	return nil
}

func (m *memStore) Delete(_ context.Context, keys ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objects, k)
	}
	return nil
}

func (m *memStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://s3.example/" + key + "?sig=x", nil
}

type testEnv struct {
	app    *App
	store  *memStore
	server *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := openDB(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `TRUNCATE users, sessions, photos CASCADE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.pool.Close)
	tmpl, err := loadTemplates()
	if err != nil {
		t.Fatal(err)
	}
	store := &memStore{objects: map[string][]byte{}}
	cfg := &Config{BaseURL: "http://test.local", S3Prefix: "sp"}
	app := &App{cfg: cfg, db: db, store: store, oauth: newOAuthConfig(cfg), tmpl: tmpl}
	srv := httptest.NewServer(app.routes())
	t.Cleanup(srv.Close)
	return &testEnv{app: app, store: store, server: srv}
}

// login creates a user and returns a client carrying their session cookie.
func (e *testEnv) login(t *testing.T, sub, email string) (*http.Client, int64) {
	ctx := context.Background()
	u, err := e.app.db.UpsertUser(ctx, sub, email, strings.Split(email, "@")[0])
	if err != nil {
		t.Fatal(err)
	}
	token := randomToken()
	if err := e.app.db.CreateSession(ctx, hashToken(token), u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: cookieTransport{token},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, u.ID
}

type cookieTransport struct{ token string }

func (c cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.token})
	return http.DefaultTransport.RoundTrip(r)
}

func (e *testEnv) upload(t *testing.T, c *http.Client, name string, file, thumb []byte) (*http.Response, map[string]string) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if thumb != nil {
		w, _ := mw.CreateFormFile("thumb", "thumb.jpg")
		w.Write(thumb)
	}
	w, _ := mw.CreateFormFile("file", name)
	w.Write(file)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/api/photos", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestUploadAndGallery(t *testing.T) {
	e := newTestEnv(t)
	alice, _ := e.login(t, "sub-alice", "alice@example.com")
	img := testJPEG(t)

	resp, out := e.upload(t, alice, "beach.jpg", img, img)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status %d: %v", resp.StatusCode, out)
	}
	id := out["id"]
	if _, ok := e.store.objects["sp/photos/"+id+".jpg"]; !ok {
		t.Errorf("original not stored; have %v", keys(e.store.objects))
	}
	if _, ok := e.store.objects["sp/thumbs/"+id+".jpg"]; !ok {
		t.Errorf("thumbnail not stored")
	}

	p, err := e.app.db.GetPhoto(context.Background(), id)
	if err != nil || p == nil {
		t.Fatalf("GetPhoto: %v %v", p, err)
	}
	if d := time.Until(p.ExpiresAt); d < 29*24*time.Hour || d > 30*24*time.Hour+time.Minute {
		t.Errorf("expires in %v, want ~30 days", d)
	}

	res, _ := alice.Get(e.server.URL + "/")
	page := readBody(t, res)
	if !strings.Contains(page, `class="grid"`) || !strings.Contains(page, "/p/"+id+"/thumb") {
		t.Errorf("gallery missing grid/photo:\n%s", page)
	}
	if !strings.Contains(page, `multiple`) {
		t.Error("file input should allow multiple selection")
	}

	res, _ = alice.Get(e.server.URL + "/p/" + id + "/thumb")
	if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "sp/thumbs/"+id) {
		t.Errorf("thumb redirect: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestUploadRejects(t *testing.T) {
	e := newTestEnv(t)
	alice, _ := e.login(t, "sub-alice", "alice@example.com")

	big := append(testJPEG(t), make([]byte, maxUploadBytes)...)
	if resp, out := e.upload(t, alice, "big.jpg", big, nil); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: status %d %v", resp.StatusCode, out)
	}
	exact := append(testJPEG(t), make([]byte, maxUploadBytes-len(testJPEG(t)))...)
	if resp, out := e.upload(t, alice, "exact.jpg", exact, nil); resp.StatusCode != http.StatusCreated {
		t.Errorf("exactly 10 MiB: status %d %v", resp.StatusCode, out)
	}
	if resp, _ := e.upload(t, alice, "x.html", []byte("<html><body>hi"), nil); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("html: status %d", resp.StatusCode)
	}

	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, _ := e.upload(t, anon, "a.jpg", testJPEG(t), nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous upload: status %d", resp.StatusCode)
	}
	if res, _ := anon.Get(e.server.URL + "/"); res.StatusCode != http.StatusFound || res.Header.Get("Location") != "/login" {
		t.Errorf("anonymous gallery: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	req, _ := http.NewRequest(http.MethodPost, e.server.URL+"/api/photos", strings.NewReader(""))
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := alice.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin upload: status %d", resp.StatusCode)
	}
}

func TestDeleteOwnership(t *testing.T) {
	e := newTestEnv(t)
	alice, _ := e.login(t, "sub-alice", "alice@example.com")
	bob, _ := e.login(t, "sub-bob", "bob@example.com")
	_, out := e.upload(t, alice, "a.jpg", testJPEG(t), nil)
	id := out["id"]

	if res, _ := bob.Post(e.server.URL+"/p/"+id+"/delete", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("bob deleting alice's photo: status %d", res.StatusCode)
	}
	if res, _ := alice.Post(e.server.URL+"/p/"+id+"/delete", "", nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("alice deleting: status %d", res.StatusCode)
	}
	if len(e.store.objects) != 0 {
		t.Errorf("objects left after delete: %v", keys(e.store.objects))
	}
}

func TestReaperRemovesExpired(t *testing.T) {
	e := newTestEnv(t)
	alice, _ := e.login(t, "sub-alice", "alice@example.com")
	_, old := e.upload(t, alice, "old.jpg", testJPEG(t), nil)
	_, fresh := e.upload(t, alice, "new.jpg", testJPEG(t), nil)
	ctx := context.Background()
	if _, err := e.app.db.pool.Exec(ctx, `UPDATE photos SET expires_at = now() - interval '1 minute' WHERE id = $1`, old["id"]); err != nil {
		t.Fatal(err)
	}

	// Expired photos are hidden immediately, before the reaper runs.
	if p, _ := e.app.db.GetPhoto(ctx, old["id"]); p != nil {
		t.Error("expired photo still visible")
	}
	n, err := e.app.reapOnce(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reapOnce = %d, %v", n, err)
	}
	if got := keys(e.store.objects); len(got) != 1 || !strings.Contains(got[0], fresh["id"]) {
		t.Errorf("objects after reap: %v", got)
	}
}

func TestPagination(t *testing.T) {
	e := newTestEnv(t)
	alice, uid := e.login(t, "sub-alice", "alice@example.com")
	_ = alice
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < pageSize+5; i++ {
		p := &Photo{ID: newID(), UserID: uid, ObjectKey: "k", Filename: "f.jpg", ContentType: "image/jpeg",
			CreatedAt: base.Add(time.Duration(i) * time.Second), ExpiresAt: time.Now().Add(time.Hour)}
		if err := e.app.db.InsertPhoto(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	res, _ := alice.Get(e.server.URL + "/")
	page := readBody(t, res)
	if strings.Count(page, `<li>`) != pageSize || !strings.Contains(page, "?before=") {
		t.Fatalf("first page: %d items", strings.Count(page, `<li>`))
	}
	next := page[strings.Index(page, "?before=")+len("?before="):]
	next = next[:strings.IndexByte(next, '"')]
	res, _ = alice.Get(e.server.URL + "/?before=" + strings.ReplaceAll(next, "&#43;", "+"))
	page = readBody(t, res)
	if n := strings.Count(page, `<li>`); n != 5 || strings.Contains(page, "?before=") {
		t.Errorf("second page: %d items (status %d)", n, res.StatusCode)
	}
}

func testJPEG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.Set(1, 1, color.Black)
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readBody(t *testing.T, r *http.Response) string {
	defer r.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(r.Body)
	return b.String()
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
