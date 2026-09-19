package store

import (
	"context"
	"errors"
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
	Address string `json:"address,omitempty"`
	// Priority says which controller should lead when more than one can:
	// lower goes first, 0 means no preference.
	Priority   int       `json:"priority"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// RecordController says this controller is running. Every controller does
// it on the same beat as the lease, leader or not, so the UI can show them
// all and say which is acting.
func (s *Store) RecordController(ctx context.Context, rec ControllerRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO controllers (name, holder, url, version, address, priority, started_at, last_seen_at)
		VALUES ($1, $2, $3, $4, coalesce(host(inet_client_addr()), ''), $5, $6, now())
		ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, url = EXCLUDED.url,
			version = EXCLUDED.version, address = EXCLUDED.address, priority = EXCLUDED.priority,
			started_at = EXCLUDED.started_at, last_seen_at = now()`,
		rec.Name, rec.Holder, rec.URL, rec.Version, rec.Priority, rec.StartedAt)
	return err
}

// Controllers lists them, the one seen most recently first.
func (s *Store) Controllers(ctx context.Context) ([]ControllerRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, holder, url, version, address, priority, started_at, last_seen_at
		FROM controllers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ControllerRecord, error) {
		var c ControllerRecord
		err := r.Scan(&c.Name, &c.Holder, &c.URL, &c.Version, &c.Address, &c.Priority, &c.StartedAt, &c.LastSeenAt)
		return c, err
	})
}

// ForgetController removes one, for a controller that's been taken out of
// the installation for good.
func (s *Store) ForgetController(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM controllers WHERE name = $1`, name)
	return err
}

// LeadershipChoice is an administrator's choice of which controller
// should lead, overriding the configured priority until it's cleared.
type LeadershipChoice struct {
	Controller string    `json:"controller"`
	ChosenBy   string    `json:"chosen_by,omitempty"`
	ChosenAt   time.Time `json:"chosen_at,omitzero"`
}

// Chosen returns the choice, or a zero value when there is none.
func (s *Store) Chosen(ctx context.Context) (LeadershipChoice, error) {
	var c LeadershipChoice
	err := s.pool.QueryRow(ctx, `SELECT controller, chosen_by, chosen_at FROM leadership_choice WHERE one`).
		Scan(&c.Controller, &c.ChosenBy, &c.ChosenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadershipChoice{}, nil
	}
	return c, err
}

// Choose says which controller should lead. It's written from whichever
// controller the person is talking to -- the standby, usually, since
// that's where someone asks for it -- and the leader reads it and steps
// aside. Nothing about the lease itself changes here.
func (s *Store) Choose(ctx context.Context, controller, by string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO leadership_choice (one, controller, chosen_by, chosen_at) VALUES (true, $1, $2, now())
		ON CONFLICT (one) DO UPDATE SET controller = EXCLUDED.controller, chosen_by = EXCLUDED.chosen_by,
			chosen_at = now()`, controller, by)
	return err
}

// ClearChoice goes back to the configured priority.
func (s *Store) ClearChoice(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM leadership_choice WHERE one`)
	return err
}
