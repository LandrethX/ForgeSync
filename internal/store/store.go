// Package store keeps ForgeSync's control-plane state in PostgreSQL.
// It never touches Forgejo's own databases.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"scenegit.org/forgesync/internal/health"
)

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL and checks the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Ping reports whether the database is reachable (used by /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// NodeRecord is a Forgejo node as registered in ForgeSync.
type NodeRecord struct {
	Name string
	URL  string
	Site string
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
			INSERT INTO nodes (name, url, site) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO UPDATE SET url = EXCLUDED.url, site = EXCLUDED.site, updated_at = now()`,
			n.Name, n.URL, n.Site)
		if err != nil {
			return fmt.Errorf("register node %s: %w", n.Name, err)
		}
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
