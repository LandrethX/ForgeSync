package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// OrgRecord is what the nodes last agreed an organization looks like.
type OrgRecord struct {
	Name        string            `json:"name"`
	BaseFields  map[string]string `json:"fields"`
	BaseTeams   string            `json:"-"`
	BaseMembers string            `json:"-"`
}

// Org returns what was agreed about an organization; an unknown one comes
// back empty, which is how a first run starts.
func (s *Store) Org(ctx context.Context, name string) (OrgRecord, error) {
	rec := OrgRecord{Name: name, BaseFields: map[string]string{}}
	err := s.pool.QueryRow(ctx, `
		SELECT base_fields, base_teams, base_members FROM organizations WHERE name = $1`, name).
		Scan(&rec.BaseFields, &rec.BaseTeams, &rec.BaseMembers)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrgRecord{Name: name, BaseFields: map[string]string{}}, nil
	}
	return rec, err
}

// SaveOrg records it.
func (s *Store) SaveOrg(ctx context.Context, rec OrgRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO organizations (name, base_fields, base_teams, base_members, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (name) DO UPDATE SET base_fields = EXCLUDED.base_fields, base_teams = EXCLUDED.base_teams,
			base_members = EXCLUDED.base_members, updated_at = now()`,
		rec.Name, rec.BaseFields, rec.BaseTeams, rec.BaseMembers)
	return err
}
