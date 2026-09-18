package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Rename is a repository renamed or transferred on its primary.
type Rename struct {
	RepositoryID string
	From, To     string
	Primary      string
}

// DetectRenames finds repositories renamed or transferred on their primary:
// gone from the primary under their name, while a repository with the same
// Forgejo id is there under another. The renamed repository keeps its
// identity (primary choice, replication state, conflicts, backups): it takes
// the new name and the primary's copy, the row created for the new name is
// dropped, and the old name becomes an alias, so copies still under it on
// other nodes stay attached until ForgeSync renames them.
//
// Like AssignPrimaries it only runs when the latest scan of every node
// succeeded, and returns ok=false otherwise.
func (s *Store) DetectRenames(ctx context.Context, nodes []string) (out []Rename, ok bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	var good int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory_scans WHERE ok AND node = ANY($1)`, nodes).Scan(&good); err != nil {
		return nil, false, err
	}
	if good < len(nodes) {
		return nil, false, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT old.id::text, old.full_name, new.id::text, new.full_name, old.primary_node
		FROM repositories old
		JOIN repository_replicas gone ON gone.repository_id = old.id AND gone.node = old.primary_node
		JOIN repository_replicas here ON here.node = gone.node AND here.forgejo_id = gone.forgejo_id
			AND here.present AND here.repository_id <> old.id
		JOIN repositories new ON new.id = here.repository_id
		WHERE NOT gone.present AND gone.last_seen_at IS NOT NULL AND gone.forgejo_id <> 0
			AND old.deleted_at IS NULL AND old.primary_node = ANY($1)
		ORDER BY old.id
		FOR UPDATE OF old`, nodes)
	if err != nil {
		return nil, false, err
	}
	type pair struct{ oldID, oldName, newID, newName, primary string }
	pairs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pair, error) {
		var p pair
		return p, r.Scan(&p.oldID, &p.oldName, &p.newID, &p.newName, &p.primary)
	})
	if err != nil {
		return nil, false, err
	}
	merged := map[string]bool{}
	for _, p := range pairs {
		if merged[p.oldID] || merged[p.newID] {
			continue // one rename per repository per round
		}
		merged[p.oldID], merged[p.newID] = true, true
		if err := mergeRename(ctx, tx, p.oldID, p.oldName, p.newID, p.newName, p.primary); err != nil {
			return nil, false, fmt.Errorf("rename %s to %s: %w", p.oldName, p.newName, err)
		}
		out = append(out, Rename{RepositoryID: p.oldID, From: p.oldName, To: p.newName, Primary: p.primary})
	}

	// Aliases go once no node has a copy under the old name.
	if _, err := tx.Exec(ctx, `
		DELETE FROM repository_aliases a WHERE NOT EXISTS (
			SELECT 1 FROM repository_replicas rr
			WHERE rr.repository_id = a.repository_id AND rr.present AND lower(rr.full_name) = a.name)`); err != nil {
		return nil, false, err
	}
	return out, true, tx.Commit(ctx)
}

func mergeRename(ctx context.Context, tx pgx.Tx, oldID, oldName, newID, newName, primary string) error {
	// The new name's row only has what the scans saw under that name. The
	// primary's copy moves over; on other nodes it only fills a gap (a
	// different repository already using the new name there stays a problem
	// for the engine to report, the old-name copy is kept).
	if _, err := tx.Exec(ctx, `
		DELETE FROM repository_replicas o USING repository_replicas n
		WHERE o.repository_id = $1 AND n.repository_id = $2 AND n.node = o.node
			AND (o.node = $3 OR NOT o.present) AND n.present`, oldID, newID, primary); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE repository_replicas n SET repository_id = $1
		WHERE n.repository_id = $2 AND NOT EXISTS (
			SELECT 1 FROM repository_replicas o WHERE o.repository_id = $1 AND o.node = n.node)`, oldID, newID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repositories WHERE id = $1`, newID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE repositories SET full_name = $2 WHERE id = $1`, oldID, newName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("repository vanished")
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO repository_aliases (name, repository_id) VALUES (lower($1), $2)
		ON CONFLICT (name) DO UPDATE SET repository_id = EXCLUDED.repository_id, created_at = now()`, oldName, oldID)
	return err
}

// RenamedTo returns the name a repository has on a node under its Forgejo
// id, if the scans found it there under a different name ("" if not).
func (s *Store) RenamedTo(ctx context.Context, repositoryID, node string) (string, error) {
	if !isUUID(repositoryID) {
		return "", ErrNotFound
	}
	var name string
	err := s.pool.QueryRow(ctx, `
		SELECT here.full_name FROM repository_replicas gone
		JOIN repository_replicas here ON here.node = gone.node AND here.forgejo_id = gone.forgejo_id
			AND here.present AND here.repository_id <> gone.repository_id
		WHERE gone.repository_id = $1::uuid AND gone.node = $2 AND gone.forgejo_id <> 0
		LIMIT 1`, repositoryID, node).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// PreviousName returns another name under which the inventory still lists
// the repository that is fullName on node (same Forgejo id there), e.g. the
// old name right after a rename ("" if none).
func (s *Store) PreviousName(ctx context.Context, node, fullName string) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `
		SELECT old.full_name FROM repository_replicas cur
		JOIN repository_replicas old ON old.node = cur.node AND old.forgejo_id = cur.forgejo_id
			AND old.repository_id <> cur.repository_id AND old.present
		WHERE cur.node = $1 AND lower(cur.full_name) = lower($2) AND cur.present AND cur.forgejo_id <> 0
		LIMIT 1`, node, fullName).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}
