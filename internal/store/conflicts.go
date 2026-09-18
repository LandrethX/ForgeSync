package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// FoundConflict is a conflict a check found.
type FoundConflict struct {
	RepositoryID string
	Kind         string
	Ref          string
	Details      map[string]any
}

// ConflictChange reports a conflict that was opened or cleared by a sync.
type ConflictChange struct {
	ID       int64
	FullName string
	Kind     string
	Ref      string
	Change   string // "opened" or "cleared"
}

// SyncConflicts records the outcome of a conflict check at time at. Found
// conflicts are opened, or refreshed if already open. Open conflicts of the
// checked repositories that weren't found again are cleared. Repositories not
// in checked (the check couldn't reach a conclusion) keep their conflicts.
func (s *Store) SyncConflicts(ctx context.Context, found []FoundConflict, checked []string, at time.Time) ([]ConflictChange, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var changes []ConflictChange
	for _, f := range found {
		details := f.Details
		if details == nil {
			details = map[string]any{}
		}
		var c ConflictChange
		var inserted bool
		err := tx.QueryRow(ctx, `
			WITH up AS (
				INSERT INTO conflicts (repository_id, kind, ref, state, details, detected_at, last_seen_at)
				VALUES ($1::uuid, $2, $3, 'open', $4, $5, $5)
				ON CONFLICT (repository_id, kind, ref) WHERE state = 'open'
				DO UPDATE SET details = EXCLUDED.details, last_seen_at = EXCLUDED.last_seen_at
				RETURNING id, repository_id, (xmax = 0) AS inserted
			)
			SELECT up.id, r.full_name, up.inserted FROM up JOIN repositories r ON r.id = up.repository_id`,
			f.RepositoryID, f.Kind, f.Ref, details, at).Scan(&c.ID, &c.FullName, &inserted)
		if err != nil {
			return nil, err
		}
		if inserted {
			c.Kind, c.Ref, c.Change = f.Kind, f.Ref, "opened"
			changes = append(changes, c)
		}
	}

	if len(checked) > 0 {
		rows, err := tx.Query(ctx, `
			WITH cl AS (
				UPDATE conflicts SET state = 'cleared', cleared_at = $2
				WHERE state = 'open' AND repository_id = ANY($1::uuid[]) AND last_seen_at < $2
				RETURNING id, repository_id, kind, ref
			)
			SELECT cl.id, r.full_name, cl.kind, cl.ref FROM cl JOIN repositories r ON r.id = cl.repository_id`,
			checked, at)
		if err != nil {
			return nil, err
		}
		cleared, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConflictChange, error) {
			c := ConflictChange{Change: "cleared"}
			return c, row.Scan(&c.ID, &c.FullName, &c.Kind, &c.Ref)
		})
		if err != nil {
			return nil, err
		}
		changes = append(changes, cleared...)
	}
	return changes, tx.Commit(ctx)
}

// Conflict is a stored conflict with its repository's name and primary.
type Conflict struct {
	ID             int64          `json:"id"`
	RepositoryID   string         `json:"repository_id"`
	FullName       string         `json:"full_name"`
	PrimaryNode    string         `json:"primary_node"`
	Kind           string         `json:"kind"`
	Ref            string         `json:"ref"`
	State          string         `json:"state"`
	Details        map[string]any `json:"details"`
	DetectedAt     time.Time      `json:"detected_at"`
	LastSeenAt     time.Time      `json:"last_seen_at"`
	ClearedAt      *time.Time     `json:"cleared_at,omitempty"`
	AcknowledgedBy string         `json:"acknowledged_by,omitempty"`
	AcknowledgedAt *time.Time     `json:"acknowledged_at,omitempty"`
	Note           string         `json:"note,omitempty"`
}

// ConflictFilter selects conflicts. Empty fields don't filter.
type ConflictFilter struct {
	State        string // "open", "cleared" or ""
	RepositoryID string
	Limit        int
	Offset       int
}

const conflictColumns = `c.id, c.repository_id::text, r.full_name, coalesce(r.primary_node, ''), c.kind, c.ref,
	c.state, c.details, c.detected_at, c.last_seen_at, c.cleared_at, c.acknowledged_by, c.acknowledged_at, c.note`

func scanConflict(row pgx.CollectableRow) (Conflict, error) {
	var c Conflict
	err := row.Scan(&c.ID, &c.RepositoryID, &c.FullName, &c.PrimaryNode, &c.Kind, &c.Ref, &c.State, &c.Details,
		&c.DetectedAt, &c.LastSeenAt, &c.ClearedAt, &c.AcknowledgedBy, &c.AcknowledgedAt, &c.Note)
	return c, err
}

// Conflicts returns matching conflicts (open ones first, newest first), the
// number matching, and the count per state ignoring the state filter.
func (s *Store) Conflicts(ctx context.Context, f ConflictFilter) ([]Conflict, int, map[string]int, error) {
	if f.RepositoryID != "" && !isUUID(f.RepositoryID) {
		return []Conflict{}, 0, map[string]int{}, nil
	}
	counts := map[string]int{"open": 0, "cleared": 0}
	rows, err := s.pool.Query(ctx, `
		SELECT state, count(*) FROM conflicts
		WHERE $1 = '' OR repository_id = $1::uuid GROUP BY state`, f.RepositoryID)
	if err != nil {
		return nil, 0, nil, err
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, 0, nil, err
		}
		counts[st] = n
	}
	rows.Close()

	total := counts["open"] + counts["cleared"]
	if f.State != "" {
		total = counts[f.State]
	}
	rows, err = s.pool.Query(ctx, `
		SELECT `+conflictColumns+`
		FROM conflicts c JOIN repositories r ON r.id = c.repository_id
		WHERE ($1 = '' OR c.state = $1) AND ($2 = '' OR c.repository_id = $2::uuid)
		ORDER BY c.state = 'open' DESC, c.detected_at DESC, c.id DESC
		LIMIT $3 OFFSET $4`, f.State, f.RepositoryID, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, nil, err
	}
	items, err := pgx.CollectRows(rows, scanConflict)
	if items == nil {
		items = []Conflict{}
	}
	return items, total, counts, err
}

// ConflictByID returns one conflict, or ErrNotFound.
func (s *Store) ConflictByID(ctx context.Context, id int64) (Conflict, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+conflictColumns+`
		FROM conflicts c JOIN repositories r ON r.id = c.repository_id WHERE c.id = $1`, id)
	if err != nil {
		return Conflict{}, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanConflict)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conflict{}, ErrNotFound
	}
	return c, err
}

// AcknowledgeConflict records who looked at a conflict and their note.
func (s *Store) AcknowledgeConflict(ctx context.Context, id int64, actor, note string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE conflicts SET acknowledged_by = $2, acknowledged_at = $3, note = $4 WHERE id = $1`,
		id, actor, at, note)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OpenConflicts counts open conflicts.
func (s *Store) OpenConflicts(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM conflicts WHERE state = 'open'`).Scan(&n)
	return n, err
}
