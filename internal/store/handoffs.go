package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Handoff is a diverged branch handed to the owner as a pull request.
type Handoff struct {
	ID           int64      `json:"id"`
	RepositoryID string     `json:"repository_id"`
	Ref          string     `json:"ref"`
	SHA          string     `json:"sha"`
	PrimaryNode  string     `json:"primary_node"`
	Nodes        []string   `json:"nodes"`
	Branch       string     `json:"branch"`
	PRNumber     int64      `json:"pr_number"`
	PRURL        string     `json:"pr_url"`
	State        string     `json:"state"`
	OpenedAt     time.Time  `json:"opened_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	BackupUntil  *time.Time `json:"backup_until,omitempty"`
}

const handoffColumns = `id, repository_id::text, ref, sha, primary_node, nodes, branch, pr_number, pr_url, state,
	opened_at, decided_at, backup_until`

func scanHandoff(row pgx.CollectableRow) (Handoff, error) {
	var h Handoff
	err := row.Scan(&h.ID, &h.RepositoryID, &h.Ref, &h.SHA, &h.PrimaryNode, &h.Nodes, &h.Branch, &h.PRNumber,
		&h.PRURL, &h.State, &h.OpenedAt, &h.DecidedAt, &h.BackupUntil)
	return h, err
}

// Handoffs returns a repository's hand-offs; active only: those still open
// or keeping a backup.
func (s *Store) Handoffs(ctx context.Context, repositoryID string, activeOnly bool) ([]Handoff, error) {
	if !isUUID(repositoryID) {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+handoffColumns+` FROM conflict_handoffs
		WHERE repository_id = $1::uuid AND (NOT $2 OR state IN ('open', 'kept_primary'))
		ORDER BY id`, repositoryID, activeOnly)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanHandoff)
}

// SaveHandoff records a new hand-off and returns its id.
func (s *Store) SaveHandoff(ctx context.Context, h Handoff) (int64, error) {
	var id int64
	h.Nodes = noNilNodes(h.Nodes)
	err := s.pool.QueryRow(ctx, `
		INSERT INTO conflict_handoffs (repository_id, ref, sha, primary_node, nodes, branch, pr_number, pr_url, state, opened_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, 'open', $9)
		RETURNING id`, h.RepositoryID, h.Ref, h.SHA, h.PrimaryNode, h.Nodes, h.Branch, h.PRNumber, h.PRURL, h.OpenedAt).Scan(&id)
	return id, err
}

// noNilNodes keeps an empty list an empty list: the column is NOT NULL, and
// a hand-off whose last replica has been reset has no nodes left.
func noNilNodes(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// UpdateHandoff stores a hand-off's head, state, nodes and dates.
func (s *Store) UpdateHandoff(ctx context.Context, h Handoff) error {
	h.Nodes = noNilNodes(h.Nodes)
	_, err := s.pool.Exec(ctx, `
		UPDATE conflict_handoffs SET sha = $2, state = $3, nodes = $4, decided_at = $5, backup_until = $6 WHERE id = $1`,
		h.ID, h.SHA, h.State, h.Nodes, h.DecidedAt, h.BackupUntil)
	return err
}
