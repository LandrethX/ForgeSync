package store

import (
	"context"
	"time"
)

// RepoItem is a label or milestone with its copy on each node.
type RepoItem struct {
	ID           string            `json:"id"`
	RepositoryID string            `json:"repository_id"`
	Kind         string            `json:"kind"` // label or milestone
	OriginNode   string            `json:"origin_node"`
	Base         map[string]string `json:"fields"`
	DeletedAt    *time.Time        `json:"deleted_at,omitempty"`
	Copies       map[string]int64  `json:"copies"` // node -> Forgejo id
}

// RepoItems returns a repository's labels and milestones.
func (s *Store) RepoItems(ctx context.Context, repositoryID string) ([]RepoItem, error) {
	if !isUUID(repositoryID) {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.id::text, i.kind, i.origin_node, i.base, i.deleted_at, c.node, c.forgejo_id
		FROM repo_items i LEFT JOIN repo_item_copies c ON c.item_id = i.id
		WHERE i.repository_id = $1::uuid
		ORDER BY i.kind, i.id, c.node`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoItem
	for rows.Next() {
		var it RepoItem
		var node *string
		var fid *int64
		if err := rows.Scan(&it.ID, &it.Kind, &it.OriginNode, &it.Base, &it.DeletedAt, &node, &fid); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != it.ID {
			it.RepositoryID, it.Copies = repositoryID, map[string]int64{}
			out = append(out, it)
		}
		if node != nil {
			out[len(out)-1].Copies[*node] = *fid
		}
	}
	return out, rows.Err()
}

// SaveRepoItem inserts (empty ID) or updates an item and replaces its
// copies. It returns the ID.
func (s *Store) SaveRepoItem(ctx context.Context, it RepoItem) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if it.ID == "" {
		err = tx.QueryRow(ctx, `
			INSERT INTO repo_items (repository_id, kind, origin_node, base, deleted_at)
			VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text`,
			it.RepositoryID, it.Kind, it.OriginNode, it.Base, it.DeletedAt).Scan(&it.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE repo_items SET base = $2, deleted_at = $3 WHERE id = $1::uuid`, it.ID, it.Base, it.DeletedAt)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repo_item_copies WHERE item_id = $1::uuid`, it.ID); err != nil {
		return "", err
	}
	for node, fid := range it.Copies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO repo_item_copies (item_id, kind, node, forgejo_id) VALUES ($1::uuid, $2, $3, $4)`,
			it.ID, it.Kind, node, fid); err != nil {
			return "", err
		}
	}
	return it.ID, tx.Commit(ctx)
}

// DeleteRepoItem forgets a label or milestone.
func (s *Store) DeleteRepoItem(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM repo_items WHERE id = $1::uuid`, id)
	return err
}
