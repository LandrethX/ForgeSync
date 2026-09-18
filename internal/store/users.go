package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScannedUser is one SceneID account as a node scan found it.
type ScannedUser struct {
	Login     string
	ForgejoID int64
	Sub       string // SceneID subject (Forgejo's login_name)
	Created   time.Time
}

// RecordNodeUsers stores the SceneID accounts a successful scan of a node
// found; accounts it no longer has are marked absent.
func (s *Store) RecordNodeUsers(ctx context.Context, node string, finished time.Time, users []ScannedUser) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Every node's scan upserts the same users at about the same time; taking
	// the row locks in one order keeps them from deadlocking.
	users = append([]ScannedUser(nil), users...)
	sort.Slice(users, func(i, j int) bool { return users[i].Sub < users[j].Sub })
	for _, u := range users {
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (sub, login) VALUES ($1, $2)
			ON CONFLICT (sub) DO UPDATE SET login = EXCLUDED.login
			RETURNING id`, u.Sub, u.Login).Scan(&id); err != nil {
			return fmt.Errorf("register user %s: %w", u.Login, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_accounts (user_id, node, login, forgejo_id, forgejo_created_at, present, checked_at)
			VALUES ($1, $2, $3, $4, $5, true, $6)
			ON CONFLICT (user_id, node) DO UPDATE SET login = EXCLUDED.login, forgejo_id = EXCLUDED.forgejo_id,
				forgejo_created_at = EXCLUDED.forgejo_created_at, present = true, checked_at = EXCLUDED.checked_at`,
			id, node, u.Login, u.ForgejoID, nullTime(u.Created), finished); err != nil {
			return fmt.Errorf("record user %s on %s: %w", u.Login, node, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE user_accounts SET present = false, checked_at = $2
		WHERE node = $1 AND checked_at < $2`, node, finished); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// NoteCreatedAccount records that ForgeSync created an account, so it never
// counts as where the user registered.
func (s *Store) NoteCreatedAccount(ctx context.Context, node, login string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO created_accounts (node, login) VALUES ($1, lower($2)) ON CONFLICT DO NOTHING`, node, login)
	return err
}

// HomeAssignment is a user's primary site set automatically.
type HomeAssignment struct {
	UserID string
	Login  string
	Node   string
}

// PrimaryChange is a repository primary set or changed automatically.
type PrimaryChange struct {
	RepositoryID string
	FullName     string
	From, To     string
	Reason       string // "owner" or "origin"
}

// Assignments is what AssignPrimaries changed.
type Assignments struct {
	Homes     []HomeAssignment
	Primaries []PrimaryChange
}

// AssignPrimaries applies the primary-site rules:
//
//  1. A SceneID user without a primary site gets their registration site:
//     the node where their account was created first, not counting accounts
//     ForgeSync created.
//  2. A repository owned by a SceneID user with a primary site gets that
//     site as its primary, and follows it when it changes.
//  3. Any other repository without a primary (owned by an organization or
//     a local admin) gets its origin: the node where it was created first,
//     ignoring pull mirrors.
//
// A primary an Administrator chose is never changed. It only runs when the
// latest scan of every node succeeded (a node that's down could hide an
// earlier account or copy); otherwise it returns ok=false and changes nothing.
func (s *Store) AssignPrimaries(ctx context.Context, nodes []string) (out Assignments, ok bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx)

	var good int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inventory_scans WHERE ok AND node = ANY($1)`, nodes).Scan(&good); err != nil {
		return out, false, err
	}
	if good < len(nodes) {
		return out, false, nil
	}

	// 1. Registration sites. Accounts ForgeSync created only count if there's
	// nothing else (e.g. the original was deleted).
	rows, err := tx.Query(ctx, `
		UPDATE users u SET home_node = pick.node, home_source = 'registration'
		FROM (
			SELECT DISTINCT ON (a.user_id) a.user_id, a.node
			FROM user_accounts a JOIN users u2 ON u2.id = a.user_id
			WHERE u2.home_node IS NULL AND a.present AND a.node = ANY($1)
			ORDER BY a.user_id,
				EXISTS (SELECT 1 FROM created_accounts c WHERE c.node = a.node AND c.login = lower(a.login)),
				a.forgejo_created_at NULLS LAST, a.node
		) pick
		WHERE u.id = pick.user_id AND u.home_node IS NULL
		RETURNING u.id::text, u.login, u.home_node`, nodes)
	if err != nil {
		return out, false, err
	}
	out.Homes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (HomeAssignment, error) {
		var h HomeAssignment
		return h, r.Scan(&h.UserID, &h.Login, &h.Node)
	})
	if err != nil {
		return out, false, err
	}

	// 2. Repositories follow their owner's primary site.
	rows, err = tx.Query(ctx, `
		WITH want AS (
			SELECT r.id, r.full_name, coalesce(r.primary_node, '') AS old, u.home_node AS new
			FROM repositories r
			JOIN users u ON lower(u.login) = lower(split_part(r.full_name, '/', 1))
			WHERE r.primary_source <> 'manual' AND u.home_node = ANY($1)
				AND (r.primary_node IS DISTINCT FROM u.home_node OR r.primary_source <> 'owner')
				-- Pull mirrors are Forgejo's own copies; a repository that's only
				-- a mirror isn't ForgeSync's to replicate.
				AND EXISTS (SELECT 1 FROM repository_replicas rr
					WHERE rr.repository_id = r.id AND rr.present AND NOT rr.mirror)
			FOR UPDATE OF r
		)
		UPDATE repositories r SET primary_node = want.new, primary_source = 'owner'
		FROM want WHERE r.id = want.id
		RETURNING r.id::text, r.full_name, want.old, want.new`, nodes)
	if err != nil {
		return out, false, err
	}
	owner, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PrimaryChange, error) {
		c := PrimaryChange{Reason: "owner"}
		return c, r.Scan(&c.RepositoryID, &c.FullName, &c.From, &c.To)
	})
	if err != nil {
		return out, false, err
	}

	// 3. Everything else without a primary: its origin.
	rows, err = tx.Query(ctx, `
		UPDATE repositories r SET primary_node = pick.node, primary_source = 'origin'
		FROM (
			SELECT DISTINCT ON (rr.repository_id) rr.repository_id, rr.node
			FROM repository_replicas rr JOIN repositories r2 ON r2.id = rr.repository_id
			WHERE r2.primary_node IS NULL AND rr.present AND NOT rr.mirror
				AND rr.forgejo_created_at IS NOT NULL AND rr.node = ANY($1)
			ORDER BY rr.repository_id, rr.forgejo_created_at, rr.node
		) pick
		WHERE r.id = pick.repository_id AND r.primary_node IS NULL
		RETURNING r.id::text, r.full_name, '', r.primary_node`, nodes)
	if err != nil {
		return out, false, err
	}
	origin, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PrimaryChange, error) {
		c := PrimaryChange{Reason: "origin"}
		return c, r.Scan(&c.RepositoryID, &c.FullName, &c.From, &c.To)
	})
	if err != nil {
		return out, false, err
	}
	out.Primaries = append(owner, origin...)
	return out, true, tx.Commit(ctx)
}

// UserAccount is a user's account on one node.
type UserAccount struct {
	Node           string     `json:"node"`
	Login          string     `json:"login"`
	Present        bool       `json:"present"`
	ForgejoCreated *time.Time `json:"forgejo_created_at,omitempty"`
	CreatedByUs    bool       `json:"created_by_forgesync"`
}

// UserRecord is a SceneID user, their primary site and their accounts.
type UserRecord struct {
	ID          string        `json:"id"`
	Sub         string        `json:"sub"`
	Login       string        `json:"login"`
	HomeNode    string        `json:"home_node"`
	HomeSource  string        `json:"home_source"`
	FirstSeenAt time.Time     `json:"first_seen_at"`
	Accounts    []UserAccount `json:"accounts"`
}

// Users returns every known SceneID user, by login.
func (s *Store) Users(ctx context.Context) ([]UserRecord, error) { return s.users(ctx, "") }

// User returns one user by id, or ErrNotFound.
func (s *Store) User(ctx context.Context, id string) (UserRecord, error) {
	if !isUUID(id) {
		return UserRecord{}, ErrNotFound
	}
	us, err := s.users(ctx, id)
	if err != nil {
		return UserRecord{}, err
	}
	if len(us) == 0 {
		return UserRecord{}, ErrNotFound
	}
	return us[0], nil
}

func (s *Store) users(ctx context.Context, id string) ([]UserRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id::text, u.sub, u.login, coalesce(u.home_node, ''), u.home_source, u.first_seen_at,
			a.node, a.login, a.present, a.forgejo_created_at,
			EXISTS (SELECT 1 FROM created_accounts c WHERE c.node = a.node AND c.login = lower(a.login))
		FROM users u LEFT JOIN user_accounts a ON a.user_id = u.id
		WHERE $1 = '' OR u.id = $1::uuid
		ORDER BY lower(u.login), u.id, a.node`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRecord
	for rows.Next() {
		var u UserRecord
		var node, login *string
		var present, ours *bool
		var created *time.Time
		if err := rows.Scan(&u.ID, &u.Sub, &u.Login, &u.HomeNode, &u.HomeSource, &u.FirstSeenAt,
			&node, &login, &present, &created, &ours); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != u.ID {
			u.Accounts = []UserAccount{}
			out = append(out, u)
		}
		if node != nil {
			last := &out[len(out)-1]
			last.Accounts = append(last.Accounts, UserAccount{Node: *node, Login: *login, Present: *present,
				ForgejoCreated: created, CreatedByUs: *ours})
		}
	}
	return out, rows.Err()
}

// SetUserHome records an Administrator's choice of primary site for a user
// and returns the previous one. Their repositories follow on the next
// assignment round, unless an Administrator chose theirs.
func (s *Store) SetUserHome(ctx context.Context, id, node string) (string, error) {
	if node == "" {
		return "", errors.New("a user's primary site can't be cleared")
	}
	if !isUUID(id) {
		return "", ErrNotFound
	}
	var prev string
	err := s.pool.QueryRow(ctx, `
		UPDATE users u SET home_node = $2, home_source = 'manual'
		FROM (SELECT id, coalesce(home_node, '') AS prev FROM users WHERE id = $1::uuid FOR UPDATE) old
		WHERE u.id = old.id
		RETURNING old.prev`, id, node).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return prev, err
}
