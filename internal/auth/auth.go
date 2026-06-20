// Package auth handles the single-admin login model for xdev: first-run admin
// creation, password verification, server-side sessions stored in sqlite, and
// the HTTP middleware that protects the UI. CSRF protection is bound to each
// session and checked on unsafe (non-GET) requests.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"xdev/internal/store"
)

const (
	sessionCookie = "xdev_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// Service bundles the auth operations over the store.
type Service struct {
	store  *store.Store
	secure bool // set Secure flag on cookies (true when served over HTTPS)
}

// New creates an auth Service. Pass secure=true when xdev is served over TLS.
func New(st *store.Store, secure bool) *Service {
	return &Service{store: st, secure: secure}
}

// NeedsSetup reports whether no admin exists yet (first run).
func (s *Service) NeedsSetup() (bool, error) {
	n, err := s.store.UserCount()
	return n == 0, err
}

// CreateAdmin creates the first (admin) user. Refuses if one already exists —
// this is the guard behind the web /setup first-run flow. To add *additional*
// admins later, use CreateUser.
func (s *Service) CreateAdmin(email, password string) (store.User, error) {
	need, err := s.NeedsSetup()
	if err != nil {
		return store.User{}, err
	}
	if !need {
		return store.User{}, errors.New("admin already exists")
	}
	return s.CreateUser(email, password)
}

// CreateUser creates an account (admin) without the first-run guard, so the app
// can have multiple admins. All users have equal access — there are no roles
// yet, so every account is effectively an admin.
func (s *Service) CreateUser(email, password string) (store.User, error) {
	email = NormalizeEmail(email)
	if email == "" || len(password) < 8 {
		return store.User{}, errors.New("email required and password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return store.User{}, err
	}
	return s.store.CreateUser(email, string(hash))
}

// SetPassword resets an existing account's password (e.g. recovery, or an admin
// resetting another's). Returns store.ErrNotFound if the email isn't known.
func (s *Service) SetPassword(email, password string) error {
	email = NormalizeEmail(email)
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.store.UpdatePassword(email, string(hash))
}

// NormalizeEmail lower-cases and trims an email so lookups and logins agree.
func NormalizeEmail(email string) string {
	return strings.TrimSpace(strings.ToLower(email))
}

// Authenticate verifies an email/password pair, returning the user on success.
func (s *Service) Authenticate(email, password string) (store.User, error) {
	email = NormalizeEmail(email)
	u, err := s.store.UserByEmail(email)
	if err != nil {
		// Run a dummy compare to keep timing roughly constant whether or not
		// the email exists, then report a generic error.
		bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinv"), []byte(password))
		return store.User{}, errors.New("invalid credentials")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return store.User{}, errors.New("invalid credentials")
	}
	return u, nil
}

// StartSession creates a session for the user and writes the session cookie.
// It returns the session's CSRF token so the UI can embed it in forms.
func (s *Service) StartSession(w http.ResponseWriter, userID int64) (string, error) {
	token := randomToken()
	csrf := randomToken()
	expires := time.Now().Add(sessionTTL)
	if err := s.store.CreateSession(token, userID, csrf, expires); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return csrf, nil
}

// EndSession deletes the current session and clears the cookie (logout).
func (s *Service) EndSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// --- request context plumbing ------------------------------------------------

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

// UserFrom returns the authenticated user attached by RequireAuth, if any.
func UserFrom(r *http.Request) (store.User, bool) {
	u, ok := r.Context().Value(ctxUser).(store.User)
	return u, ok
}

// SessionFrom returns the session attached by RequireAuth, if any.
func SessionFrom(r *http.Request) (store.Session, bool) {
	sess, ok := r.Context().Value(ctxSession).(store.Session)
	return sess, ok
}

// RequireAuth wraps a handler so only logged-in requests reach it. Unauthenticated
// requests are redirected to /login. For unsafe methods, it also enforces CSRF.
func (s *Service) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		sess, err := s.store.SessionByToken(c.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		user, err := s.store.UserByID(sess.UserID)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !isSafeMethod(r.Method) && r.FormValue("csrf_token") != sess.CSRFToken {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxSession, sess)
		next(w, r.WithContext(ctx))
	}
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// randomToken returns a 32-byte random hex string for cookies / CSRF tokens.
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("xdev: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
