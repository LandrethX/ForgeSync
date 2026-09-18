package api

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/auth"
)

// Sessions holds browser sessions in memory. A restart signs everyone out,
// which is acceptable for a single controller; an HA pair will need shared
// sessions instead.
type Sessions struct {
	TTL  time.Duration // absolute lifetime
	Idle time.Duration // expires after this long without requests

	mu  sync.Mutex
	m   map[string]*session
	now func() time.Time
}

type session struct {
	identity auth.Identity
	idToken  string // SceneID ID token, used as id_token_hint when signing out
	lastSeen time.Time
	expires  time.Time
}

func NewSessions(ttl, idle time.Duration) *Sessions {
	return &Sessions{TTL: ttl, Idle: idle, m: map[string]*session{}, now: time.Now}
}

// Create starts a session and returns its id and absolute expiry.
func (s *Sessions) Create(id auth.Identity, idToken string) (string, time.Time) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	key := base64.RawURLEncoding.EncodeToString(b)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if !s.valid(v, now) {
			delete(s.m, k)
		}
	}
	sess := &session{identity: id, idToken: idToken, lastSeen: now, expires: now.Add(s.TTL)}
	s.m[key] = sess
	return key, sess.expires
}

// Get returns the session and counts the call as activity for the idle timeout.
func (s *Sessions) Get(key string) (id auth.Identity, idToken string, expires time.Time, ok bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, found := s.m[key]
	if !found || !s.valid(sess, now) {
		delete(s.m, key)
		return auth.Identity{}, "", time.Time{}, false
	}
	sess.lastSeen = now
	return sess.identity, sess.idToken, sess.expires, true
}

// Valid reports whether the session exists and hasn't expired, without
// counting as activity (used by the event stream, so an open dashboard
// doesn't keep an idle session alive).
func (s *Sessions) Valid(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[key]
	return ok && s.valid(sess, s.now())
}

func (s *Sessions) Delete(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

func (s *Sessions) valid(sess *session, now time.Time) bool {
	return now.Before(sess.expires) && now.Sub(sess.lastSeen) < s.Idle
}

// loginLimiter counts failed sign-ins per client address in a fixed window.
type loginLimiter struct {
	Max    int
	Window time.Duration

	mu  sync.Mutex
	m   map[string]*window
	now func() time.Time
}

type window struct {
	start    time.Time
	failures int
}

func newLoginLimiter(max int, w time.Duration) *loginLimiter {
	return &loginLimiter{Max: max, Window: w, m: map[string]*window{}, now: time.Now}
}

// Blocked reports whether addr has used up its failures, and when it may retry.
func (l *loginLimiter) Blocked(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.m[addr]
	if !ok {
		return false, 0
	}
	left := w.start.Add(l.Window).Sub(l.now())
	if left <= 0 {
		delete(l.m, addr)
		return false, 0
	}
	return w.failures >= l.Max, left
}

func (l *loginLimiter) Fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, v := range l.m {
		if now.Sub(v.start) >= l.Window {
			delete(l.m, k)
		}
	}
	w, ok := l.m[addr]
	if !ok {
		w = &window{start: now}
		l.m[addr] = w
	}
	w.failures++
}

func (l *loginLimiter) Reset(addr string) {
	l.mu.Lock()
	delete(l.m, addr)
	l.mu.Unlock()
}
