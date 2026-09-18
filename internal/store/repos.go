package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a looked-up row doesn't exist.
var ErrNotFound = errors.New("not found")

// ScannedRepo is one repository as a node scan found it.
type ScannedRepo struct {
	FullName      string
	ForgejoID     int64
	Private       bool
	Fork          bool
	Mirror        bool
	Archived      bool
	Empty         bool
	DefaultBranch string
	HeadSHA       string
	HeadError     string
	Updated       time.Time
	Created       time.Time
}

// RecordNodeScan stores a successful scan of a node: every repository it
// found, and which previously seen ones are gone.
func (s *Store) RecordNodeScan(ctx context.Context, node string, started, finished time.Time, repos []ScannedRepo) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, r := range repos {
		var id string
		// first_seen_at is when the finding scan started, so a node scanned in
		// the same round counts as checked and a repository on only one node
		// shows as missing right away. The trade-off: one created mid-round
		// can show as missing on the other node until the next scan.
		err := tx.QueryRow(ctx, `
			INSERT INTO repositories (full_name, first_seen_at) VALUES ($1, $2)
			ON CONFLICT ((lower(full_name))) DO UPDATE SET full_name = repositories.full_name
			RETURNING id`, r.FullName, started).Scan(&id)
		if err != nil {
			return fmt.Errorf("register repository %s: %w", r.FullName, err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO repository_replicas (repository_id, node, present, forgejo_id, private, fork, mirror, archived,
				empty, default_branch, head_sha, head_error, forgejo_updated_at, forgejo_created_at, last_seen_at, checked_at)
			VALUES ($1, $2, true, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
			ON CONFLICT (repository_id, node) DO UPDATE SET
				present = true, forgejo_id = EXCLUDED.forgejo_id, private = EXCLUDED.private, fork = EXCLUDED.fork,
				mirror = EXCLUDED.mirror, archived = EXCLUDED.archived, empty = EXCLUDED.empty,
				default_branch = EXCLUDED.default_branch, head_sha = EXCLUDED.head_sha, head_error = EXCLUDED.head_error,
				forgejo_updated_at = EXCLUDED.forgejo_updated_at, forgejo_created_at = EXCLUDED.forgejo_created_at, last_seen_at = EXCLUDED.last_seen_at,
				checked_at = EXCLUDED.checked_at`,
			id, node, r.ForgejoID, r.Private, r.Fork, r.Mirror, r.Archived, r.Empty,
			r.DefaultBranch, r.HeadSHA, r.HeadError, nullTime(r.Updated), nullTime(r.Created), finished)
		if err != nil {
			return fmt.Errorf("record %s on %s: %w", r.FullName, node, err)
		}
	}
	// Anything on this node the scan didn't touch is gone from it.
	if _, err := tx.Exec(ctx, `
		UPDATE repository_replicas SET present = false, checked_at = $2
		WHERE node = $1 AND checked_at < $2`, node, finished); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory_scans (node, started_at, finished_at, ok, error, repositories, last_success_at)
		VALUES ($1, $2, $3, true, '', $4, $3)
		ON CONFLICT (node) DO UPDATE SET started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at,
			ok = true, error = '', repositories = EXCLUDED.repositories, last_success_at = EXCLUDED.last_success_at`,
		node, started, finished, len(repos)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RecordNodeScanFailure stores a failed scan. What earlier scans found is
// kept; the UI shows it as possibly out of date.
func (s *Store) RecordNodeScanFailure(ctx context.Context, node string, started, finished time.Time, scanErr error) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO inventory_scans (node, started_at, finished_at, ok, error)
		VALUES ($1, $2, $3, false, $4)
		ON CONFLICT (node) DO UPDATE SET started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at,
			ok = false, error = EXCLUDED.error`,
		node, started, finished, scanErr.Error())
	return err
}

// NodeScan is the outcome of a node's latest inventory scan.
type NodeScan struct {
	Node          string     `json:"node"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    time.Time  `json:"finished_at"`
	OK            bool       `json:"ok"`
	Error         string     `json:"error,omitempty"`
	Repositories  int        `json:"repositories"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
}

func (s *Store) NodeScans(ctx context.Context) ([]NodeScan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT node, started_at, finished_at, ok, error, repositories, last_success_at
		FROM inventory_scans ORDER BY node`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (NodeScan, error) {
		var n NodeScan
		err := row.Scan(&n.Node, &n.StartedAt, &n.FinishedAt, &n.OK, &n.Error, &n.Repositories, &n.LastSuccessAt)
		return n, err
	})
}

// Replica is what a node's scans saw of one repository.
type Replica struct {
	Node           string     `json:"node"`
	Present        bool       `json:"present"`
	ForgejoID      int64      `json:"forgejo_id"`
	Private        bool       `json:"private"`
	Fork           bool       `json:"fork"`
	Mirror         bool       `json:"mirror"`
	Archived       bool       `json:"archived"`
	Empty          bool       `json:"empty"`
	DefaultBranch  string     `json:"default_branch"`
	HeadSHA        string     `json:"head_sha"`
	HeadError      string     `json:"head_error,omitempty"`
	ForgejoUpdated *time.Time `json:"forgejo_updated_at,omitempty"`
	ForgejoCreated *time.Time `json:"forgejo_created_at,omitempty"`
	LastSeenAt     *time.Time `json:"last_seen_at,omitempty"`
	CheckedAt      time.Time  `json:"checked_at"`
}

// RepositoryRecord is a repository and what each node has of it.
type RepositoryRecord struct {
	ID          string `json:"id"`
	FullName    string `json:"full_name"`
	PrimaryNode string `json:"primary_node"`
	// PrimarySource is how the primary was chosen: "owner" (the owner's
	// primary site), "origin" (the node it was created on first, for owners
	// without one), "manual", or "" while none is set.
	PrimarySource string    `json:"primary_source"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
	Replicas      []Replica `json:"-"`
}

// Repositories returns every known repository with its replicas, by name.
func (s *Store) Repositories(ctx context.Context) ([]RepositoryRecord, error) {
	return s.repositories(ctx, "")
}

// Repository returns one repository by id, or ErrNotFound.
func (s *Store) Repository(ctx context.Context, id string) (RepositoryRecord, error) {
	if !isUUID(id) {
		return RepositoryRecord{}, ErrNotFound
	}
	recs, err := s.repositories(ctx, id)
	if err != nil {
		return RepositoryRecord{}, err
	}
	if len(recs) == 0 {
		return RepositoryRecord{}, ErrNotFound
	}
	return recs[0], nil
}

func (s *Store) repositories(ctx context.Context, id string) ([]RepositoryRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id::text, r.full_name, coalesce(r.primary_node, ''), r.primary_source, r.first_seen_at,
			rr.node, rr.present, rr.forgejo_id, rr.private, rr.fork, rr.mirror, rr.archived, rr.empty,
			rr.default_branch, rr.head_sha, rr.head_error, rr.forgejo_updated_at, rr.forgejo_created_at,
			rr.last_seen_at, rr.checked_at
		FROM repositories r
		LEFT JOIN repository_replicas rr ON rr.repository_id = r.id
		WHERE $1 = '' OR r.id = $1::uuid
		ORDER BY lower(r.full_name), rr.node`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepositoryRecord
	for rows.Next() {
		var rec RepositoryRecord
		var node *string
		var rp Replica
		var present, private, fork, mirror, archived, empty *bool
		var forgejoID *int64
		var branch, sha, headErr *string
		var checked *time.Time
		if err := rows.Scan(&rec.ID, &rec.FullName, &rec.PrimaryNode, &rec.PrimarySource, &rec.FirstSeenAt,
			&node, &present, &forgejoID, &private, &fork, &mirror, &archived, &empty,
			&branch, &sha, &headErr, &rp.ForgejoUpdated, &rp.ForgejoCreated, &rp.LastSeenAt, &checked); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != rec.ID {
			out = append(out, rec)
		}
		if node != nil {
			rp.Node, rp.Present, rp.ForgejoID = *node, *present, *forgejoID
			rp.Private, rp.Fork, rp.Mirror, rp.Archived, rp.Empty = *private, *fork, *mirror, *archived, *empty
			rp.DefaultBranch, rp.HeadSHA, rp.HeadError, rp.CheckedAt = *branch, *sha, *headErr, *checked
			last := &out[len(out)-1]
			last.Replicas = append(last.Replicas, rp)
		}
	}
	return out, rows.Err()
}

// SetPrimary records an Administrator's choice of primary for a repository
// and returns the previous value.
func (s *Store) SetPrimary(ctx context.Context, id, node string) (string, error) {
	if node == "" {
		return "", errors.New("a repository's primary can't be cleared")
	}
	if !isUUID(id) {
		return "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var prev string
	err = tx.QueryRow(ctx, `SELECT coalesce(primary_node, '') FROM repositories WHERE id = $1::uuid FOR UPDATE`, id).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE repositories SET primary_node = $2, primary_source = 'manual' WHERE id = $1::uuid`, id, node); err != nil {
		return "", err
	}
	return prev, tx.Commit(ctx)
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
