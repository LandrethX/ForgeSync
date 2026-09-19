package issues

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
)

// Reviews are what people said about a pull request's diff: a submitted
// review with its line comments. They replicate member by member (see
// set.go), like reactions and attachments, because a review is submitted
// once and stands: one that appears anywhere is submitted on the nodes
// that lack it, as the person who wrote it, and one missing anywhere is
// deleted from the nodes that still have it.
//
// What makes the line comments carry at all is that the diff is the same
// everywhere: a comment is anchored to a path, a line and a commit, and
// git replication has already put that commit on every node.
//
// A review that is edited afterwards reads as one going and another
// arriving, so it is submitted again -- the same trade as renaming an
// attachment. Reviews still being drafted (PENDING) aren't submitted yet
// and are left alone, and dismissing one isn't copied: Forgejo's API can't
// submit a review already dismissed, so ForgeSync would put it back
// undismissed on the next run.

// review is a submitted review with its line comments, which travel
// together because Forgejo submits them together.
type review struct {
	head     forgejo.PullReview
	comments []forgejo.PullReviewComment
}

// reviewMember identifies a review across the nodes. Nothing in it is a
// node's own: the reviewer, the commit and every word are the same
// wherever it was written, so the digest is too.
func reviewMember(v review) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n%s\n%s\n", reviewerOf(v), v.head.State, v.head.CommitID, v.head.Body)
	lines := make([]string, 0, len(v.comments))
	for _, c := range v.comments {
		lines = append(lines, fmt.Sprintf("%s|%d|%d|%d|%s", c.Path, c.OldLine, c.Line, c.Extra, c.Body))
	}
	sort.Strings(lines)
	b.WriteString(strings.Join(lines, "\n"))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

func reviewerOf(v review) string {
	if v.head.Reviewer == nil {
		return ""
	}
	return v.head.Reviewer.Login
}

// submitted reports that ForgeSync should carry this review: one still
// being drafted isn't a review yet, and one from a team rather than a
// person has nobody to submit it as.
func submitted(v review) bool {
	switch v.head.State {
	case "APPROVED", "REQUEST_CHANGES", "COMMENT":
		return reviewerOf(v) != ""
	}
	return false
}

// reviewsOn is what one node has, by member.
func reviewsOn(list []review) map[string]review {
	out := map[string]review{}
	for _, v := range list {
		if !submitted(v) {
			continue
		}
		if _, taken := out[reviewMember(v)]; !taken {
			out[reviewMember(v)] = v
		}
	}
	return out
}

// reviewer submits a review on a node, or takes one off it.
type reviewer struct {
	submit func(ctx context.Context, node, as string, v review) error
	remove func(ctx context.Context, node string, v review) error
}

// syncReviews brings one pull request's reviews together and returns the
// new base. at maps a node to what it has.
func (r *run) syncReviews(ctx context.Context, ref, source, base string,
	at map[string]map[string]review, act reviewer) string {
	have := map[string]map[string]bool{}
	for n, m := range at {
		have[n] = map[string]bool{}
		for e := range m {
			have[n][e] = true
		}
	}
	plan := set.Decide(have, set.From(base))
	for _, n := range r.nodes {
		for _, e := range plan.Remove[n] {
			v := at[n][e]
			if err := act.remove(ctx, n, v); err != nil {
				r.s.log.Warn("issues: a review couldn't be removed; left for the next run", "repository", r.rec.FullName,
					"node", n, "issue", ref, "by", reviewerOf(v), "error", err)
				r.complete = false
				continue
			}
			delete(at[n], e)
			delete(have[n], e)
			r.s.log.Info("review removed", "repository", r.rec.FullName, "node", n, "issue", ref, "by", reviewerOf(v))
		}
		for _, e := range plan.Add[n] {
			v, from, ok := r.findReview(at, e)
			if !ok {
				r.s.log.Warn("issues: no node has the review to copy", "repository", r.rec.FullName,
					"issue", ref, "review", e)
				r.complete = false
				continue
			}
			who := reviewerOf(v)
			if r.s.opts.EnsureUser != nil {
				if err := r.s.opts.EnsureUser(ctx, who, source, n); err != nil {
					r.s.log.Info("issues: a reviewer can't be created on the node; left for the next run",
						"repository", r.rec.FullName, "node", n, "issue", ref, "by", who, "error", err)
					r.complete = false
					continue
				}
			}
			if err := act.submit(ctx, n, who, v); err != nil {
				r.s.log.Warn("issues: a review couldn't be copied; left for the next run", "repository", r.rec.FullName,
					"node", n, "from", from, "issue", ref, "by", who, "error", err)
				r.complete = false
				continue
			}
			if at[n] == nil {
				at[n] = map[string]review{}
			}
			at[n][e], have[n][e] = v, true
			r.s.log.Info("review copied", "repository", r.rec.FullName, "node", n, "from", from, "issue", ref,
				"by", who, "state", v.head.State, "comments", len(v.comments))
		}
	}
	return set.Settle(have, set.From(base))
}

// findReview reads a review from a node that has it, the primary first.
func (r *run) findReview(at map[string]map[string]review, member string) (review, string, bool) {
	for _, n := range r.nodes {
		if v, ok := at[n][member]; ok {
			return v, n, true
		}
	}
	return review{}, "", false
}

// asNew is a review as Forgejo takes it when it's submitted again
// elsewhere.
func asNew(v review) forgejo.NewReview {
	out := forgejo.NewReview{Event: v.head.State, Body: v.head.Body, CommitID: v.head.CommitID}
	for _, c := range v.comments {
		out.Comments = append(out.Comments, forgejo.NewReviewComment{
			Path: c.Path, Body: c.Body, OldLine: c.OldLine, NewLine: c.Line, Extra: c.Extra})
	}
	return out
}
