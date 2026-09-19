package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sessions are who is signed in. They're here rather than in a
// controller's memory so that a failover doesn't sign everyone out: the
// controllers share this database, so the session moves with the work.
//
// What's stored is a hash of the cookie's value, never the value itself,
// so this table can't be read back into a signed-in session. The identity
// is JSON to this package -- it belongs to whoever put it there.

// CreateSession records a new one and clears out any that have run out.
func (s *Store) CreateSession(ctx context.Context, hash string, identity []byte, expires time.Time, idle time.Duration) error {
	if _, err := s.PurgeSessions(ctx, idle); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (key_hash, identity, expires_at) VALUES ($1, $2, $3)`,
		hash, identity, expires)
	return err
}

// Session returns a session that hasn't run out, absolutely or through
// idleness. With touch, the call counts as activity against the idle
// limit; without it -- an open event stream, say -- it doesn't, so a page
// left open doesn't keep someone signed in forever.
func (s *Store) Session(ctx context.Context, hash string, idle time.Duration, touch bool) ([]byte, time.Time, bool, error) {
	var identity []byte
	var expires time.Time
	query := `
		SELECT identity, expires_at FROM sessions
		WHERE key_hash = $1 AND expires_at > now() AND last_used_at > now() - $2::interval`
	if touch {
		query = `
			UPDATE sessions SET last_used_at = now()
			WHERE key_hash = $1 AND expires_at > now() AND last_used_at > now() - $2::interval
			RETURNING identity, expires_at`
	}
	err := s.pool.QueryRow(ctx, query, hash, idle).Scan(&identity, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return identity, expires, true, nil
}

// DeleteSession signs one out, on every controller at once.
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE key_hash = $1`, hash)
	return err
}

// PurgeSessions removes the ones that have run out.
func (s *Store) PurgeSessions(ctx context.Context, idle time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM sessions WHERE expires_at <= now() OR last_used_at <= now() - $1::interval`, idle)
	return tag.RowsAffected(), err
}
