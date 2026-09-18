package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReplicaSync is the latest replication outcome for one replica of a repository.
type ReplicaSync struct {
	RepositoryID   string     `json:"repository_id"`
	Node           string     `json:"node"`
	State          string     `json:"state"` // synced, conflict, error, waiting, missing
	Detail         string     `json:"detail,omitempty"`
	LastAttemptAt  time.Time  `json:"last_attempt_at"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	OutOfSyncSince *time.Time `json:"out_of_sync_since,omitempty"`
	RefsUpdated    int        `json:"refs_updated"`
}

// ReplicatedRefs returns what ForgeSync last wrote to a replica.
func (s *Store) ReplicatedRefs(ctx context.Context, repositoryID, node string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ref, sha FROM replicated_refs WHERE repository_id = $1::uuid AND node = $2`, repositoryID, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := map[string]string{}
	for rows.Next() {
		var ref, sha string
		if err := rows.Scan(&ref, &sha); err != nil {
			return nil, err
		}
		refs[ref] = sha
	}
	return refs, rows.Err()
}

// ForgetReplicatedRefs drops what ForgeSync last wrote to a replica, e.g.
// after it recreated the repository there.
func (s *Store) ForgetReplicatedRefs(ctx context.Context, repositoryID, node string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM replicated_refs WHERE repository_id = $1::uuid AND node = $2`, repositoryID, node)
	return err
}

// SaveReplicaSync records a replication attempt. If refs is non-nil it
// replaces the replica's replicated refs in the same transaction.
// out_of_sync_since keeps the start of a streak of non-synced attempts.
func (s *Store) SaveReplicaSync(ctx context.Context, st ReplicaSync, refs map[string]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	synced := st.State == "synced"
	if _, err := tx.Exec(ctx, `
		INSERT INTO replica_sync (repository_id, node, state, detail, last_attempt_at, last_success_at, out_of_sync_since, refs_updated)
		VALUES ($1::uuid, $2, $3, $4, $5, CASE WHEN $6 THEN $5::timestamptz END, CASE WHEN $6 THEN NULL ELSE $5::timestamptz END, $7)
		ON CONFLICT (repository_id, node) DO UPDATE SET
			state = EXCLUDED.state, detail = EXCLUDED.detail, last_attempt_at = EXCLUDED.last_attempt_at,
			last_success_at = CASE WHEN $6 THEN EXCLUDED.last_attempt_at ELSE replica_sync.last_success_at END,
			out_of_sync_since = CASE WHEN $6 THEN NULL
				ELSE coalesce(replica_sync.out_of_sync_since, EXCLUDED.last_attempt_at) END,
			refs_updated = EXCLUDED.refs_updated`,
		st.RepositoryID, st.Node, st.State, st.Detail, st.LastAttemptAt, synced, st.RefsUpdated); err != nil {
		return err
	}
	if refs != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM replicated_refs WHERE repository_id = $1::uuid AND node = $2`,
			st.RepositoryID, st.Node); err != nil {
			return err
		}
		for ref, sha := range refs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO replicated_refs (repository_id, node, ref, sha, updated_at) VALUES ($1::uuid, $2, $3, $4, $5)`,
				st.RepositoryID, st.Node, ref, sha, st.LastAttemptAt); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// ReplicaSyncs returns the replication state of one repository's replicas.
func (s *Store) ReplicaSyncs(ctx context.Context, repositoryID string) ([]ReplicaSync, error) {
	if !isUUID(repositoryID) {
		return []ReplicaSync{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT repository_id::text, node, state, detail, last_attempt_at, last_success_at, out_of_sync_since, refs_updated
		FROM replica_sync WHERE repository_id = $1::uuid ORDER BY node`, repositoryID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReplicaSync, error) {
		var x ReplicaSync
		err := r.Scan(&x.RepositoryID, &x.Node, &x.State, &x.Detail, &x.LastAttemptAt, &x.LastSuccessAt, &x.OutOfSyncSince, &x.RefsUpdated)
		return x, err
	})
}

// ReplicationCounts counts replicas by state, for repositories that still
// have a primary.
func (s *Store) ReplicationCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rs.state, count(*) FROM replica_sync rs
		JOIN repositories r ON r.id = rs.repository_id
		WHERE r.primary_node IS NOT NULL AND r.primary_node <> rs.node
		GROUP BY rs.state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		counts[st] = n
	}
	return counts, rows.Err()
}
