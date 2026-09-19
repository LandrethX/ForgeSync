package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ControllerRecord is one ForgeSync controller sharing this database.
type ControllerRecord struct {
	Name    string `json:"name"`
	Holder  string `json:"holder"`
	URL     string `json:"url,omitempty"`
	Version string `json:"version,omitempty"`
	// Address is where the database sees this controller connect from:
	// its real address, rather than what it believes about itself.
	Address    string    `json:"address,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// RecordController says this controller is running. Every controller does
// it on the same beat as the lease, leader or not, so the UI can show them
// all and say which is acting.
func (s *Store) RecordController(ctx context.Context, rec ControllerRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO controllers (name, holder, url, version, address, started_at, last_seen_at)
		VALUES ($1, $2, $3, $4, coalesce(host(inet_client_addr()), ''), $5, now())
		ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, url = EXCLUDED.url,
			version = EXCLUDED.version, address = EXCLUDED.address,
			started_at = EXCLUDED.started_at, last_seen_at = now()`,
		rec.Name, rec.Holder, rec.URL, rec.Version, rec.StartedAt)
	return err
}

// Controllers lists them, the one seen most recently first.
func (s *Store) Controllers(ctx context.Context) ([]ControllerRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, holder, url, version, address, started_at, last_seen_at
		FROM controllers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ControllerRecord, error) {
		var c ControllerRecord
		err := r.Scan(&c.Name, &c.Holder, &c.URL, &c.Version, &c.Address, &c.StartedAt, &c.LastSeenAt)
		return c, err
	})
}

// ForgetController removes one, for a controller that's been taken out of
// the installation for good.
func (s *Store) ForgetController(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM controllers WHERE name = $1`, name)
	return err
}
