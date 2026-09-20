// Package store keeps ForgeSync's control-plane state in PostgreSQL.
// It never touches Forgejo's own databases.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"scenegit.org/forgesync/internal/health"
)

// Store is ForgeSync's state in PostgreSQL. Every controller shares one,
// which is what lets either of them serve and only one of them act.
type Store struct {
	pool *pgxpool.Pool
}

// ErrStandby says the server answering is a PostgreSQL standby: it
// replies to every read and accepts no write. That is not a database
// ForgeSync can work in, and it is worth telling apart from an
// unreachable one, because the two are fixed differently.
var ErrStandby = errors.New("the server is a standby and takes no writes; " +
	"name every server in database.url and add target_session_attrs=read-write, " +
	"or point it at whatever fronts them")

// Open connects to PostgreSQL and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close gives the connection pool back.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable and is the one taking
// writes (used by /readyz, /metrics and the overview).
//
// Asking whether it answers isn't enough. Where the database is more
// than one server with a promotion tool over them, a controller can end
// up talking to a standby: it answers every read, so the pages look
// right, while the lease can't be renewed and nothing is replicated --
// and ForgeSync would be reporting itself healthy the whole time.
// pg_is_in_recovery is the server's own answer to which one it is, so
// ask it rather than trusting the address.
func (s *Store) Ping(ctx context.Context) error {
	var standby bool
	if err := s.pool.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&standby); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if standby {
		return fmt.Errorf("database: %w", ErrStandby)
	}
	return nil
}

// NodeRecord is a Forgejo node as registered in ForgeSync.
type NodeRecord struct {
	Name string
	URL  string
	Site string
	// ServiceUser is the local admin the token belongs to, and
	// SceneIDSourceID the id of the SceneID login source on that node.
	ServiceUser     string
	SceneIDSourceID int64
	// SealedToken is the node's API token, sealed with the controller's
	// node key (internal/secret). Nil means this node's token still comes
	// from the config file.
	SealedToken []byte
	// Source is 'config' or 'api', for people reading the table; AddedBy
	// names who added one through the API.
	Source  string
	AddedBy string
	// RemovedAt is set on a node that has been retired. Nodes() leaves
	// those out; nothing watches, scans or replicates to one.
	RemovedAt *time.Time
}

// SyncNodes registers the configured nodes, updating URL and site of
// existing ones. Nodes missing from the config are kept, not deleted:
// removing a node is an explicit, audited operation.
func (s *Store) SyncNodes(ctx context.Context, nodes []NodeRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, n := range nodes {
		_, err := tx.Exec(ctx, `
			INSERT INTO nodes (name, url, site, service_user, sceneid_source_id)
			VALUES ($1, $2, $3, COALESCE(NULLIF($4, ''), 'forgesync'), $5)
			ON CONFLICT (name) DO UPDATE SET
				url = EXCLUDED.url, site = EXCLUDED.site,
				service_user = EXCLUDED.service_user,
				sceneid_source_id = EXCLUDED.sceneid_source_id,
				updated_at = now()`,
			n.Name, n.URL, n.Site, n.ServiceUser, n.SceneIDSourceID)
		if err != nil {
			return fmt.Errorf("register node %s: %w", n.Name, err)
		}
	}
	return tx.Commit(ctx)
}

// Nodes is every node ForgeSync knows, in name order. Whichever of them
// carry a sealed token are the ones that no longer need the config file.
func (s *Store) Nodes(ctx context.Context) ([]NodeRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, url, site, service_user, sceneid_source_id, sealed_token, source, added_by
		FROM nodes WHERE removed_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeRecord
	for rows.Next() {
		var n NodeRecord
		if err := rows.Scan(&n.Name, &n.URL, &n.Site, &n.ServiceUser, &n.SceneIDSourceID,
			&n.SealedToken, &n.Source, &n.AddedBy); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SaveNode writes a node with its credentials, adding it or updating what
// is there. A nil SealedToken leaves whatever token the row already has,
// so changing a node's address doesn't mean re-entering its token.
func (s *Store) SaveNode(ctx context.Context, n NodeRecord) error {
	if n.ServiceUser == "" {
		n.ServiceUser = "forgesync"
	}
	if n.Source == "" {
		n.Source = "config"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (name, url, site, service_user, sceneid_source_id, sealed_token, source, added_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (name) DO UPDATE SET
			url = EXCLUDED.url, site = EXCLUDED.site, service_user = EXCLUDED.service_user,
			sceneid_source_id = EXCLUDED.sceneid_source_id,
			sealed_token = COALESCE(EXCLUDED.sealed_token, nodes.sealed_token),
			source = EXCLUDED.source, added_by = EXCLUDED.added_by,
			removed_at = NULL, updated_at = now()`,
		n.Name, n.URL, n.Site, n.ServiceUser, n.SceneIDSourceID, n.SealedToken, n.Source, n.AddedBy)
	if err != nil {
		return fmt.Errorf("save node %s: %w", n.Name, err)
	}
	return nil
}

// RetireNode takes a node out of the installation: nothing watches, scans
// or replicates to it any more, and its current health is forgotten.
//
// The row stays. Its health transitions are part of the history, which
// nothing may edit, and they point at it; and what ForgeSync learned about
// repositories there is worth keeping, because a node taken out and put
// back should not come back a stranger. SaveNode clears the mark.
func (s *Store) RetireNode(ctx context.Context, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE nodes SET removed_at = now(), updated_at = now()
		WHERE name = $1 AND removed_at IS NULL`, name)
	if err != nil {
		return fmt.Errorf("retire node %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("retire node %s: no such node", name)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM node_status WHERE node = $1`, name); err != nil {
		return fmt.Errorf("retire node %s: %w", name, err)
	}
	return tx.Commit(ctx)
}

// RecordNodeStatus stores the node's latest status and, when the state
// changed, a row in the transition history. It implements health.Recorder.
func (s *Store) RecordNodeStatus(ctx context.Context, st health.Status, prev health.State) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
		INSERT INTO node_status (node, state, version, last_checked, last_seen, failing_since, consecutive_failures, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (node) DO UPDATE SET
			state = EXCLUDED.state, version = EXCLUDED.version, last_checked = EXCLUDED.last_checked,
			last_seen = EXCLUDED.last_seen, failing_since = EXCLUDED.failing_since,
			consecutive_failures = EXCLUDED.consecutive_failures, last_error = EXCLUDED.last_error`,
		st.Node, st.State, st.Version, st.LastChecked, st.LastSeen, st.FailingSince, st.ConsecutiveFailures, st.LastError)
	if err != nil {
		return fmt.Errorf("record status of %s: %w", st.Node, err)
	}
	if st.State != prev {
		_, err = tx.Exec(ctx, `
			INSERT INTO node_state_transitions (node, from_state, to_state, at, error)
			VALUES ($1, $2, $3, $4, $5)`,
			st.Node, prev, st.State, st.LastChecked, st.LastError)
		if err != nil {
			return fmt.Errorf("record transition of %s: %w", st.Node, err)
		}
	}
	return tx.Commit(ctx)
}

// NodeStates returns the state each node was last seen in. A controller
// starting up takes these as its starting point, so a restart isn't
// recorded as every node changing from UNKNOWN to HEALTHY: the history is
// meant to show what the nodes did, not what ForgeSync did.
func (s *Store) NodeStates(ctx context.Context) (map[string]health.State, error) {
	rows, err := s.pool.Query(ctx, `SELECT node, state FROM node_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]health.State{}
	for rows.Next() {
		var node string
		var state health.State
		if err := rows.Scan(&node, &state); err != nil {
			return nil, err
		}
		out[node] = state
	}
	return out, rows.Err()
}

// Transition is one change of a node's health state.
type Transition struct {
	ID    int64        `json:"id"`
	Node  string       `json:"node"`
	From  health.State `json:"from"`
	To    health.State `json:"to"`
	At    time.Time    `json:"at"`
	Error string       `json:"error,omitempty"`
}

// Transitions returns up to limit state changes, newest first. An empty node
// means all nodes.
func (s *Store) Transitions(ctx context.Context, node string, limit int) ([]Transition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, node, from_state, to_state, at, error FROM node_state_transitions
		WHERE $1 = '' OR node = $1
		ORDER BY id DESC LIMIT $2`, node, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Transition{}
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.ID, &t.Node, &t.From, &t.To, &t.At, &t.Error); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Audit appends an entry to the audit log.
func (s *Store) Audit(ctx context.Context, actor, action, target string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, details) VALUES ($1, $2, $3, $4)`,
		actor, action, target, details)
	return err
}
