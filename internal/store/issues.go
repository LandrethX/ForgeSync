package store

import (
	"context"
	"time"
)

// IssueCopy is an issue's copy on one node.
type IssueCopy struct {
	Number    int64 `json:"number"`
	ForgejoID int64 `json:"forgejo_id"`
}

// IssueRecord is one replicated issue: its identity, the base of the
// three-way merge, and its copies per node.
type IssueRecord struct {
	ID           string    `json:"id"`
	RepositoryID string    `json:"repository_id"`
	OriginNode   string    `json:"origin_node"`
	Author       string    `json:"author"`
	CreatedAt    time.Time `json:"created_at"`
	BaseTitle    string    `json:"title"`
	BaseBody     string    `json:"-"`
	BaseState    string    `json:"state"`
	// BaseLabels is the sorted, comma-separated ids of its labels' items;
	// BaseMilestone its milestone's item id or ""; BaseAssignees the sorted,
	// comma-separated logins of its assignees.
	BaseLabels    string `json:"-"`
	BaseMilestone string `json:"-"`
	BaseAssignees string `json:"-"`
	// BaseReactions is the sorted "<login>:<content>" pairs of its
	// reactions, comma-separated.
	BaseReactions string               `json:"-"`
	DeletedAt     *time.Time           `json:"deleted_at,omitempty"` // deleted on the primary
	Copies        map[string]IssueCopy `json:"copies"`
}

// CommentRecord is one replicated comment.
type CommentRecord struct {
	ID         string    `json:"id"`
	IssueID    string    `json:"issue_id"`
	OriginNode string    `json:"origin_node"`
	Author     string    `json:"author"`
	CreatedAt  time.Time `json:"created_at"`
	BaseBody   string    `json:"-"`
	// BaseReactions is as on an issue: sorted "<login>:<content>" pairs.
	BaseReactions string           `json:"-"`
	DeletedAt     *time.Time       `json:"deleted_at,omitempty"` // deleted on the primary
	Copies        map[string]int64 `json:"copies"`               // node -> Forgejo comment id
}

// Issues returns a repository's replicated issues and their comments.
func (s *Store) Issues(ctx context.Context, repositoryID string) ([]IssueRecord, []CommentRecord, error) {
	if !isUUID(repositoryID) {
		return nil, nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.id::text, i.origin_node, i.author, i.created_at, i.base_title, i.base_body, i.base_state, i.deleted_at,
			i.base_labels, i.base_milestone, i.base_assignees, i.base_reactions, c.node, c.number, c.forgejo_id
		FROM issues i LEFT JOIN issue_copies c ON c.issue_id = i.id
		WHERE i.repository_id = $1::uuid
		ORDER BY i.created_at, i.id, c.node`, repositoryID)
	if err != nil {
		return nil, nil, err
	}
	var issues []IssueRecord
	for rows.Next() {
		var r IssueRecord
		var node *string
		var number, fid *int64
		if err := rows.Scan(&r.ID, &r.OriginNode, &r.Author, &r.CreatedAt, &r.BaseTitle, &r.BaseBody, &r.BaseState, &r.DeletedAt,
			&r.BaseLabels, &r.BaseMilestone, &r.BaseAssignees, &r.BaseReactions, &node, &number, &fid); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if len(issues) == 0 || issues[len(issues)-1].ID != r.ID {
			r.RepositoryID, r.Copies = repositoryID, map[string]IssueCopy{}
			issues = append(issues, r)
		}
		if node != nil {
			issues[len(issues)-1].Copies[*node] = IssueCopy{Number: *number, ForgejoID: *fid}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT m.id::text, m.issue_id::text, m.origin_node, m.author, m.created_at, m.base_body, m.base_reactions,
			m.deleted_at, c.node, c.forgejo_id
		FROM issue_comments m JOIN issues i ON i.id = m.issue_id
		LEFT JOIN issue_comment_copies c ON c.comment_id = m.id
		WHERE i.repository_id = $1::uuid
		ORDER BY m.created_at, m.id, c.node`, repositoryID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var comments []CommentRecord
	for rows.Next() {
		var c CommentRecord
		var node *string
		var fid *int64
		if err := rows.Scan(&c.ID, &c.IssueID, &c.OriginNode, &c.Author, &c.CreatedAt, &c.BaseBody, &c.BaseReactions,
			&c.DeletedAt, &node, &fid); err != nil {
			return nil, nil, err
		}
		if len(comments) == 0 || comments[len(comments)-1].ID != c.ID {
			c.Copies = map[string]int64{}
			comments = append(comments, c)
		}
		if node != nil {
			comments[len(comments)-1].Copies[*node] = *fid
		}
	}
	return issues, comments, rows.Err()
}

// SaveIssue inserts (empty ID) or updates an issue and replaces its copies.
// It returns the ID.
func (s *Store) SaveIssue(ctx context.Context, r IssueRecord) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if r.ID == "" {
		err = tx.QueryRow(ctx, `
			INSERT INTO issues (repository_id, origin_node, author, created_at, base_title, base_body, base_state, deleted_at,
				base_labels, base_milestone, base_assignees, base_reactions)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id::text`,
			r.RepositoryID, r.OriginNode, r.Author, r.CreatedAt, r.BaseTitle, r.BaseBody, r.BaseState, r.DeletedAt,
			r.BaseLabels, r.BaseMilestone, r.BaseAssignees, r.BaseReactions).Scan(&r.ID)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE issues SET base_title = $2, base_body = $3, base_state = $4, deleted_at = $5,
				base_labels = $6, base_milestone = $7, base_assignees = $8, base_reactions = $9, updated_at = now()
			WHERE id = $1::uuid`,
			r.ID, r.BaseTitle, r.BaseBody, r.BaseState, r.DeletedAt, r.BaseLabels, r.BaseMilestone, r.BaseAssignees,
			r.BaseReactions)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM issue_copies WHERE issue_id = $1::uuid`, r.ID); err != nil {
		return "", err
	}
	for node, c := range r.Copies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO issue_copies (issue_id, node, number, forgejo_id) VALUES ($1::uuid, $2, $3, $4)`,
			r.ID, node, c.Number, c.ForgejoID); err != nil {
			return "", err
		}
	}
	return r.ID, tx.Commit(ctx)
}

// DeleteIssueRecord forgets an issue and its comments.
func (s *Store) DeleteIssueRecord(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM issues WHERE id = $1::uuid`, id)
	return err
}

// SaveComment inserts (empty ID) or updates a comment and replaces its
// copies. It returns the ID.
func (s *Store) SaveComment(ctx context.Context, c CommentRecord) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if c.ID == "" {
		err = tx.QueryRow(ctx, `
			INSERT INTO issue_comments (issue_id, origin_node, author, created_at, base_body, deleted_at, base_reactions)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
			c.IssueID, c.OriginNode, c.Author, c.CreatedAt, c.BaseBody, c.DeletedAt, c.BaseReactions).Scan(&c.ID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE issue_comments SET base_body = $2, deleted_at = $3, base_reactions = $4
			WHERE id = $1::uuid`, c.ID, c.BaseBody, c.DeletedAt, c.BaseReactions)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM issue_comment_copies WHERE comment_id = $1::uuid`, c.ID); err != nil {
		return "", err
	}
	for node, fid := range c.Copies {
		if _, err := tx.Exec(ctx, `INSERT INTO issue_comment_copies (comment_id, node, forgejo_id) VALUES ($1::uuid, $2, $3)`,
			c.ID, node, fid); err != nil {
			return "", err
		}
	}
	return c.ID, tx.Commit(ctx)
}

// DeleteCommentRecord forgets a comment.
func (s *Store) DeleteCommentRecord(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM issue_comments WHERE id = $1::uuid`, id)
	return err
}
