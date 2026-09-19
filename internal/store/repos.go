package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	return s.recordRepos(ctx, node, started, finished, repos, true)
}

// recordRepos records repositories a node has; full means repos is
// everything it has (a scan), so the rest are gone and the scan is recorded.
func (s *Store) recordRepos(ctx context.Context, node string, started, finished time.Time, repos []ScannedRepo, full bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	names := map[string]bool{}
	for _, r := range repos {
		names[strings.ToLower(r.FullName)] = true
	}
	for _, r := range repos {
		var id, current string
		// A copy under a renamed repository's old name, on a node other than
		// its primary, belongs to that repository (until ForgeSync renames
		// it). Not if this node also has the new name: then it's another
		// repository that happens to have the old name.
		err := tx.QueryRow(ctx, `
			SELECT r.id, r.full_name FROM repository_aliases a JOIN repositories r ON r.id = a.repository_id
			WHERE a.name = lower($1) AND coalesce(r.primary_node, '') <> $2`, r.FullName, node).Scan(&id, &current)
		if err == nil && names[strings.ToLower(current)] {
			id = ""
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up aliases of %s: %w", r.FullName, err)
		}
		if id == "" {
			// first_seen_at is when the finding scan started, so a node scanned in
			// the same round counts as checked and a repository on only one node
			// shows as missing right away. The trade-off: one created mid-round
			// can show as missing on the other node until the next scan.
			err = tx.QueryRow(ctx, `
				INSERT INTO repositories (full_name, first_seen_at) VALUES ($1, $2)
				ON CONFLICT ((lower(full_name))) DO UPDATE SET full_name = repositories.full_name
				RETURNING id`, r.FullName, started).Scan(&id)
			if err != nil {
				return fmt.Errorf("register repository %s: %w", r.FullName, err)
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO repository_replicas (repository_id, node, present, forgejo_id, private, fork, mirror, archived,
				empty, default_branch, head_sha, head_error, forgejo_updated_at, forgejo_created_at, last_seen_at, checked_at, full_name)
			VALUES ($1, $2, true, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14, $15)
			ON CONFLICT (repository_id, node) DO UPDATE SET
				present = true, forgejo_id = EXCLUDED.forgejo_id, private = EXCLUDED.private, fork = EXCLUDED.fork,
				mirror = EXCLUDED.mirror, archived = EXCLUDED.archived, empty = EXCLUDED.empty,
				default_branch = EXCLUDED.default_branch, head_sha = EXCLUDED.head_sha, head_error = EXCLUDED.head_error,
				forgejo_updated_at = EXCLUDED.forgejo_updated_at, forgejo_created_at = EXCLUDED.forgejo_created_at, full_name = EXCLUDED.full_name, last_seen_at = EXCLUDED.last_seen_at,
				checked_at = EXCLUDED.checked_at`,
			id, node, r.ForgejoID, r.Private, r.Fork, r.Mirror, r.Archived, r.Empty,
			r.DefaultBranch, r.HeadSHA, r.HeadError, nullTime(r.Updated), nullTime(r.Created), finished, r.FullName)
		if err != nil {
			return fmt.Errorf("record %s on %s: %w", r.FullName, node, err)
		}
	}
	if !full {
		return tx.Commit(ctx)
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
	// FullName is the copy's name on this node. It differs from the
	// repository's while a rename on the primary isn't applied here yet.
	FullName   string     `json:"full_name"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CheckedAt  time.Time  `json:"checked_at"`
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
	// DeletedAt is when ForgeSync found it deleted on its primary.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// BaseCollaborators is the sorted "<login>:<permission>" pairs of the
	// people it's shared with, as last agreed everywhere; BaseProtection
	// the "<rule>:<digest>" members of its branch protection rules.
	BaseCollaborators string `json:"-"`
	BaseProtection    string `json:"-"`
	// BaseMetadata is the repository's settings as last agreed, and
	// BaseTopics its topics as sorted members.
	BaseMetadata map[string]string `json:"-"`
	BaseTopics   string            `json:"-"`
	Replicas     []Replica         `json:"-"`
}

// SetRepositoryCollaborators records what the nodes now agree the
// repository is shared with.
func (s *Store) SetRepositoryCollaborators(ctx context.Context, id, value string) error {
	_, err := s.pool.Exec(ctx, `UPDATE repositories SET base_collaborators = $2 WHERE id = $1::uuid`, id, value)
	return err
}

// SetRepositoryProtection records the branch protection rules the nodes
// now agree on.
func (s *Store) SetRepositoryProtection(ctx context.Context, id, value string) error {
	_, err := s.pool.Exec(ctx, `UPDATE repositories SET base_protection = $2 WHERE id = $1::uuid`, id, value)
	return err
}

// SetRepositoryMetadata records the settings and topics the nodes now
// agree on.
func (s *Store) SetRepositoryMetadata(ctx context.Context, id string, fields map[string]string, topics string) error {
	if fields == nil {
		fields = map[string]string{}
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE repositories SET base_metadata = $2, base_topics = $3 WHERE id = $1::uuid`, id, fields, topics)
	return err
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
		SELECT r.id::text, r.full_name, coalesce(r.primary_node, ''), r.primary_source, r.first_seen_at, r.deleted_at,
			r.base_collaborators, r.base_protection, r.base_metadata, r.base_topics, rr.node, rr.present, rr.forgejo_id, rr.private, rr.fork, rr.mirror, rr.archived, rr.empty,
			rr.default_branch, rr.head_sha, rr.head_error, rr.forgejo_updated_at, rr.forgejo_created_at,
			rr.last_seen_at, rr.checked_at, rr.full_name
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
		var fullName *string
		if err := rows.Scan(&rec.ID, &rec.FullName, &rec.PrimaryNode, &rec.PrimarySource, &rec.FirstSeenAt, &rec.DeletedAt,
			&rec.BaseCollaborators, &rec.BaseProtection, &rec.BaseMetadata, &rec.BaseTopics, &node, &present, &forgejoID, &private, &fork, &mirror, &archived, &empty,
			&branch, &sha, &headErr, &rp.ForgejoUpdated, &rp.ForgejoCreated, &rp.LastSeenAt, &checked, &fullName); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != rec.ID {
			out = append(out, rec)
		}
		if node != nil {
			rp.Node, rp.Present, rp.ForgejoID = *node, *present, *forgejoID
			rp.Private, rp.Fork, rp.Mirror, rp.Archived, rp.Empty = *private, *fork, *mirror, *archived, *empty
			rp.DefaultBranch, rp.HeadSHA, rp.HeadError, rp.CheckedAt = *branch, *sha, *headErr, *checked
			rp.FullName = *fullName
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

// RepositoryIDByName returns the repository with this name, or whose old
// name this is ("" if none).
func (s *Store) RepositoryIDByName(ctx context.Context, fullName string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text FROM repositories WHERE lower(full_name) = lower($1)
		UNION ALL
		SELECT repository_id::text FROM repository_aliases WHERE name = lower($1)
		LIMIT 1`, fullName).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RecordRepoObservation stores what a node has of one repository right now,
// outside a full scan (e.g. after a webhook): found or not. It doesn't touch
// the node's other repositories or its scan status.
func (s *Store) RecordRepoObservation(ctx context.Context, node string, at time.Time, fullName string, found bool, r ScannedRepo) error {
	if !found {
		_, err := s.pool.Exec(ctx, `
			UPDATE repository_replicas rr SET present = false, checked_at = $3
			FROM repositories r
			WHERE rr.repository_id = r.id AND rr.node = $1 AND lower(rr.full_name) = lower($2) AND rr.present`,
			node, fullName, at)
		return err
	}
	r.FullName = fullName
	return s.recordRepos(ctx, node, at, at, []ScannedRepo{r}, false)
}
