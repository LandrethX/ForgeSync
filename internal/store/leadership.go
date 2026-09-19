package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Lease is the leadership of the ForgeSync installation: which controller
// is acting, and until when. Now is the database's clock when it was read,
// so a controller can judge its own lease without trusting its own clock
// against another's.
type Lease struct {
	Holder     string    `json:"holder"`
	Name       string    `json:"name"`
	URL        string    `json:"url,omitempty"`
	AcquiredAt time.Time `json:"acquired_at"`
	RenewedAt  time.Time `json:"renewed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Now        time.Time `json:"-"`
}

// Held reports that the lease is still in force.
func (l Lease) Held() bool { return l.Holder != "" && l.ExpiresAt.After(l.Now) }

// For is how much longer the lease lasts.
func (l Lease) For() time.Duration {
	if !l.Held() {
		return 0
	}
	return l.ExpiresAt.Sub(l.Now)
}

const leaseColumns = `holder, name, url, acquired_at, renewed_at, expires_at, now()`

func scanLease(row pgx.Row) (Lease, error) {
	var l Lease
	err := row.Scan(&l.Holder, &l.Name, &l.URL, &l.AcquiredAt, &l.RenewedAt, &l.ExpiresAt, &l.Now)
	return l, err
}

// AcquireLease takes the lease for holder, renews it if holder already has
// it, or leaves it alone and returns the lease in force. A lease is only
// taken over once it has expired by the database's clock, which every
// controller shares.
func (s *Store) AcquireLease(ctx context.Context, holder, name, url string, ttl time.Duration) (Lease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx, `
		INSERT INTO leadership (id, holder, name, url, acquired_at, renewed_at, expires_at)
		VALUES (true, $1, $2, $3, now(), now(), now() + $4::interval)
		ON CONFLICT (id) DO UPDATE SET
			holder = EXCLUDED.holder,
			name = EXCLUDED.name,
			url = EXCLUDED.url,
			-- Keep acquired_at while the same controller keeps the lease.
			acquired_at = CASE WHEN leadership.holder = EXCLUDED.holder
				THEN leadership.acquired_at ELSE now() END,
			renewed_at = now(),
			expires_at = now() + $4::interval
			WHERE leadership.holder = EXCLUDED.holder OR leadership.expires_at <= now()
		RETURNING `+leaseColumns, holder, name, url, ttl))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.Leadership(ctx) // someone else has it
	}
	return l, err
}

// Leadership returns the lease as it stands, or an empty one if there has
// never been a leader.
func (s *Store) Leadership(ctx context.Context) (Lease, error) {
	l, err := scanLease(s.pool.QueryRow(ctx, `SELECT `+leaseColumns+` FROM leadership WHERE id`))
	if errors.Is(err, pgx.ErrNoRows) {
		var now time.Time
		if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return Lease{}, err
		}
		return Lease{Now: now}, nil
	}
	return l, err
}

// ReleaseLease gives the lease up when this controller stops, so the other
// one takes over at once instead of waiting for it to run out.
func (s *Store) ReleaseLease(ctx context.Context, holder string) error {
	_, err := s.pool.Exec(ctx, `UPDATE leadership SET expires_at = now() WHERE id AND holder = $1`, holder)
	return err
}
