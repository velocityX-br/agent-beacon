// Package auth implements the browser authentication modes for the dashboard:
// password (bcrypt), proxy-header (trusted reverse proxy), oidc (OpenID Connect
// authorization-code), and none (open). It manages opaque session cookies for
// authenticated browser sessions. Agent PSK auth is separate and lives in the
// server package.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// Mode is a browser auth provider identifier.
type Mode string

const (
	ModePassword    Mode = "password"
	ModeProxyHeader Mode = "proxy-header"
	ModeOIDC        Mode = "oidc"
	ModeNone        Mode = "none"
)

const cookieName = "agent_beacon_session"

// Session is a single authenticated browser session.
type Session struct {
	Token   string
	Subject string // who: username, proxy header value, or oidc subject
	Expires time.Time
}

// Store holds active browser sessions in memory (single-replica design).
type Store struct {
	mu       sync.RWMutex
	sessions map[string]Session
	ttl      time.Duration
}

// NewStore creates a session store; sessions expire after ttl.
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Store{sessions: make(map[string]Session), ttl: ttl}
}

// Issue creates a new session for subject and returns its opaque token.
func (s *Store) Issue(subject string) (Session, error) {
	tok, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	sess := Session{Token: tok, Subject: subject, Expires: time.Now().Add(s.ttl)}
	s.mu.Lock()
	s.sessions[tok] = sess
	s.mu.Unlock()
	return sess, nil
}

// Lookup returns the session for a token if it exists and has not expired.
func (s *Store) Lookup(tok string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[tok]
	s.mu.RUnlock()
	if !ok {
		return Session{}, false
	}
	if time.Now().After(sess.Expires) {
		s.Revoke(tok)
		return Session{}, false
	}
	return sess, true
}

// Revoke deletes a session by token.
func (s *Store) Revoke(tok string) {
	s.mu.Lock()
	delete(s.sessions, tok)
	s.mu.Unlock()
}

// SetCookie writes the session cookie on the response. secure marks it Secure
// (set when the request arrived over TLS).
func SetCookie(w http.ResponseWriter, sess Session, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    sess.Token,
		Path:     "/",
		Expires:  sess.Expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie on the response.
func ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// CookieToken returns the session token from the request cookie, if present.
func CookieToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
