package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "lens_session"

type session struct {
	User    string
	Expires time.Time
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	secret   []byte
	duration time.Duration
}

func newSessionStore(secret string, duration time.Duration) *sessionStore {
	return &sessionStore{sessions: make(map[string]session), secret: []byte(secret), duration: duration}
}

func (s *sessionStore) create(w http.ResponseWriter, r *http.Request, user string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	signature := s.sign(token)
	expires := time.Now().Add(s.duration)
	s.mu.Lock()
	s.sessions[token] = session{User: user, Expires: expires}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token + "." + signature, Path: "/",
		Expires: expires, MaxAge: int(s.duration.Seconds()), HttpOnly: true,
		Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (s *sessionStore) user(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 || !hmac.Equal([]byte(parts[1]), []byte(s.sign(parts[0]))) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[parts[0]]
	if !ok || time.Now().After(value.Expires) {
		delete(s.sessions, parts[0])
		return "", false
	}
	return value.User, true
}

func (s *sessionStore) remove(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if parts := strings.Split(cookie.Value, "."); len(parts) == 2 {
			s.mu.Lock()
			delete(s.sessions, parts[0])
			s.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *sessionStore) sign(token string) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func passwordMatches(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}
