package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	sessionCookie = "sp_session"
	stateCookie   = "sp_oauth_state"
	userinfoURL   = "https://openidconnect.googleapis.com/v1/userinfo"
)

type ctxKey struct{}

func userFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

func newOAuthConfig(c *Config) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.GoogleClientID,
		ClientSecret: c.GoogleClientSecret,
		RedirectURL:  c.BaseURL + "/auth/callback",
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint:     google.Endpoint,
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth", MaxAge: 600,
		HttpOnly: true, Secure: a.cfg.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.SetAuthURLParam("prompt", "select_account")), http.StatusFound)
}

func (a *App) handleCallback(w http.ResponseWriter, r *http.Request) {
	sc, err := r.Cookie(stateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(sc.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "invalid OAuth state, please try signing in again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth", MaxAge: -1})

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		log.Printf("oauth exchange: %v", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	info, err := fetchUserinfo(ctx, a.oauth.Client(ctx, tok))
	if err != nil {
		log.Printf("userinfo: %v", err)
		http.Error(w, "sign-in failed", http.StatusBadGateway)
		return
	}
	if !info.EmailVerified || !a.cfg.emailAllowed(info.Email) {
		a.render(w, "denied.html", map[string]any{"Email": info.Email})
		return
	}
	user, err := a.db.UpsertUser(ctx, info.Sub, info.Email, info.Name)
	if err != nil {
		a.serverError(w, err)
		return
	}
	token := randomToken()
	expires := time.Now().Add(sessionTTL)
	if err := a.db.CreateSession(ctx, hashToken(token), user.ID, expires); err != nil {
		a.serverError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: a.cfg.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.db.DeleteSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type googleUserinfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func fetchUserinfo(ctx context.Context, client *http.Client) (*googleUserinfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var info googleUserinfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	if info.Sub == "" {
		return nil, fmt.Errorf("userinfo missing sub")
	}
	return &info, nil
}

// withUser attaches the signed-in user (if any) to the request context.
func (a *App) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			u, err := a.db.SessionUser(r.Context(), hashToken(c.Value))
			if err != nil {
				a.serverError(w, err)
				return
			}
			if u != nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireUser rejects anonymous requests: pages redirect to the login screen,
// API calls get a 401.
func requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusFound)
			} else {
				http.Error(w, "not signed in", http.StatusUnauthorized)
			}
			return
		}
		next(w, r)
	}
}

// sameOrigin blocks cross-site state-changing requests. SameSite=Lax cookies
// already cover most of this; the Origin check is belt and braces.
func (a *App) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != a.cfg.BaseURL {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}
