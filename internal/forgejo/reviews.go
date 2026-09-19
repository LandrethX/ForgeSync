package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// PullReview is a submitted review on a pull request. CommitID is the
// commit it was made against, which is the same on every node because the
// git side is.
type PullReview struct {
	ID        int64     `json:"id"`
	Reviewer  *User     `json:"user"`
	State     string    `json:"state"` // APPROVED, REQUEST_CHANGES, COMMENT, PENDING
	Body      string    `json:"body"`
	CommitID  string    `json:"commit_id"`
	Dismissed bool      `json:"dismissed"`
	Comments  int       `json:"comments_count"`
	Submitted time.Time `json:"submitted_at"`
}

// PullReviewComment is one remark on a line of the diff. Line is the line
// in the new file and OldLine the one in the old, whichever side it's on;
// Forgejo calls them position and original_position when it hands them
// over and new_position and old_position when it takes them.
type PullReviewComment struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	Poster   *User  `json:"user"`
	Path     string `json:"path"`
	CommitID string `json:"commit_id"`
	Line     int64  `json:"position"`
	OldLine  int64  `json:"original_position"`
	Extra    int64  `json:"extra_lines_count"`
}

// NewReview submits a review, with its line comments in the same call.
type NewReview struct {
	Event    string             `json:"event"` // APPROVED, REQUEST_CHANGES or COMMENT
	Body     string             `json:"body"`
	CommitID string             `json:"commit_id"`
	Comments []NewReviewComment `json:"comments,omitempty"`
}

// NewReviewComment is one line comment in a review being submitted.
type NewReviewComment struct {
	Path    string `json:"path"`
	Body    string `json:"body"`
	OldLine int64  `json:"old_position"`
	NewLine int64  `json:"new_position"`
	Extra   int64  `json:"extra_lines_count"`
}

// PullReviews lists a pull request's reviews.
func (c *Client) PullReviews(ctx context.Context, owner, repo string, number int64) ([]PullReview, error) {
	var out []PullReview
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(owner, repo), number), true, nil, &out)
	return out, err
}

// PullReviewComments lists the line comments in one review.
func (c *Client) PullReviewComments(ctx context.Context, owner, repo string, number, review int64) ([]PullReviewComment, error) {
	var out []PullReviewComment
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/pulls/%d/reviews/%d/comments", repoPath(owner, repo), number, review), true, nil, &out)
	return out, err
}

// CreatePullReview submits a review as the client's user (Sudo picks who).
func (c *Client) CreatePullReview(ctx context.Context, owner, repo string, number int64, r NewReview) (PullReview, error) {
	var out PullReview
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(owner, repo), number), true, r, &out)
	return out, err
}

// DeletePullReview removes one. Already gone is not an error.
func (c *Client) DeletePullReview(ctx context.Context, owner, repo string, number, review int64) error {
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("%s/pulls/%d/reviews/%d", repoPath(owner, repo), number, review), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}
