package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/auth"
)

// SessionStore is where sessions live: the database every controller
// share, so signing in on one and being served by the other works, and a
// failover doesn't sign anyone out.
type SessionStore interface {
	CreateSession(ctx context.Context, hash string, identity []byte, expires time.Time, idle time.Duration) error
	Session(ctx context.Context, hash string, idle time.Duration, touch bool) ([]byte, time.Time, bool, error)
	DeleteSession(ctx context.Context, hash string) error
}

// Sessions are the signed-in browsers. The cookie carries a random value;
// what's stored is its SHA-256, so the table can't be read back into a
// session. A session ends at TTL whatever happens, or after Idle without
// a request.
type Sessions struct {
	TTL  time.Duration // absolute lifetime
	Idle time.Duration // expires after this long without requests

	db  SessionStore
	log *slog.Logger
}

// NewSessions keeps sessions in db: ttl is how long one lasts whatever
// happens, idle how long it survives without a request.
func NewSessions(ttl, idle time.Duration, db SessionStore, log *slog.Logger) *Sessions {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sessions{TTL: ttl, Idle: idle, db: db, log: log}
}

// keyHash is what's stored for a cookie's value.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Create starts a session and returns the cookie's value and when it ends.
func (s *Sessions) Create(ctx context.Context, id auth.Identity) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	key := base64.RawURLEncoding.EncodeToString(b)
	expires := time.Now().Add(s.TTL).UTC()
	identity, err := json.Marshal(id)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := s.db.CreateSession(ctx, keyHash(key), identity, expires, s.Idle); err != nil {
		return "", time.Time{}, err
	}
	return key, expires, nil
}

// Get returns the session and counts the call as activity against the
// idle limit. A database that can't be reached means nobody is signed in,
// which is the safe way round.
func (s *Sessions) Get(ctx context.Context, key string) (auth.Identity, time.Time, bool) {
	return s.read(ctx, key, true)
}

// Valid reports whether the session is still good, without counting as
// activity (the event stream, so an open dashboard doesn't keep an idle
// session alive).
func (s *Sessions) Valid(ctx context.Context, key string) bool {
	_, _, ok := s.read(ctx, key, false)
	return ok
}

func (s *Sessions) read(ctx context.Context, key string, touch bool) (auth.Identity, time.Time, bool) {
	identity, expires, ok, err := s.db.Session(ctx, keyHash(key), s.Idle, touch)
	if err != nil {
		s.log.Error("reading the session failed", "error", err)
		return auth.Identity{}, time.Time{}, false
	}
	if !ok {
		return auth.Identity{}, time.Time{}, false
	}
	var id auth.Identity
	if err := json.Unmarshal(identity, &id); err != nil {
		s.log.Error("a stored session couldn't be read", "error", err)
		return auth.Identity{}, time.Time{}, false
	}
	return id, expires, true
}

// Delete signs out, on every controller at once.
func (s *Sessions) Delete(ctx context.Context, key string) {
	if err := s.db.DeleteSession(ctx, keyHash(key)); err != nil {
		s.log.Error("deleting the session failed", "error", err)
	}
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

// Fail counts one failed sign-in against addr, and forgets windows that
// have run out.
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

// Reset forgets addr's failures, which a successful sign-in does.
func (l *loginLimiter) Reset(addr string) {
	l.mu.Lock()
	delete(l.m, addr)
	l.mu.Unlock()
}
