package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Archive is a copy of a deleted repository kept on one node.
type Archive struct {
	ID           int64      `json:"id"`
	RepositoryID string     `json:"repository_id"`
	Node         string     `json:"node"`
	OriginalName string     `json:"original_name"`
	ArchivedName string     `json:"archived_name"`
	State        string     `json:"state"`
	ArchivedAt   time.Time  `json:"archived_at"`
	DeleteAfter  *time.Time `json:"delete_after,omitempty"`
}

// MarkRepositoryDeleted records that a repository was deleted on its
// primary, unless that's already recorded.
func (s *Store) MarkRepositoryDeleted(ctx context.Context, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE repositories SET deleted_at = $2 WHERE id = $1::uuid AND deleted_at IS NULL`, id, at)
	return err
}

// UndeleteRepository clears the deleted mark, e.g. after the repository was
// created again on its primary.
func (s *Store) UndeleteRepository(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE repositories SET deleted_at = NULL WHERE id = $1::uuid`, id)
	return err
}

// DeleteRepository forgets a repository and everything recorded about it
// (the audit log keeps its history).
func (s *Store) DeleteRepository(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM repositories WHERE id = $1::uuid`, id)
	return err
}

// Archives returns a repository's archived copies, oldest first.
func (s *Store) Archives(ctx context.Context, repositoryID string) ([]Archive, error) {
	if !isUUID(repositoryID) {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, repository_id::text, node, original_name, archived_name, state, archived_at, delete_after
		FROM repository_archives WHERE repository_id = $1::uuid ORDER BY id`, repositoryID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Archive, error) {
		var a Archive
		err := r.Scan(&a.ID, &a.RepositoryID, &a.Node, &a.OriginalName, &a.ArchivedName, &a.State, &a.ArchivedAt, &a.DeleteAfter)
		return a, err
	})
}

// SaveArchive inserts an archive (ID 0) or updates its state and dates.
// It returns the id.
func (s *Store) SaveArchive(ctx context.Context, a Archive) (int64, error) {
	if a.ID == 0 {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO repository_archives (repository_id, node, original_name, archived_name, state, archived_at, delete_after)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7) RETURNING id`,
			a.RepositoryID, a.Node, a.OriginalName, a.ArchivedName, a.State, a.ArchivedAt, a.DeleteAfter).Scan(&a.ID)
		return a.ID, err
	}
	_, err := s.pool.Exec(ctx, `UPDATE repository_archives SET state = $2, delete_after = $3 WHERE id = $1`,
		a.ID, a.State, a.DeleteAfter)
	return a.ID, err
}
