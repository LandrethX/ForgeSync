package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// PackageOwnerRecord is what the nodes last agreed one owner's packages
// are. Packages belong to an owner, not to a repository, so this is keyed
// by name like an organization's record.
type PackageOwnerRecord struct {
	Owner string
	Base  string
}

// PackageOwner returns what was agreed about an owner's packages; an
// unknown one comes back empty, which is how a first run starts.
func (s *Store) PackageOwner(ctx context.Context, owner string) (PackageOwnerRecord, error) {
	rec := PackageOwnerRecord{Owner: owner}
	err := s.pool.QueryRow(ctx, `SELECT base_packages FROM package_owners WHERE owner = $1`, owner).Scan(&rec.Base)
	if errors.Is(err, pgx.ErrNoRows) {
		return PackageOwnerRecord{Owner: owner}, nil
	}
	return rec, err
}

// SetPackageOwner records it.
func (s *Store) SetPackageOwner(ctx context.Context, owner, base string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO package_owners (owner, base_packages, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (owner) DO UPDATE SET base_packages = EXCLUDED.base_packages, updated_at = now()`,
		owner, base)
	return err
}
