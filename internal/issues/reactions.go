package issues

import (
	"context"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
)

// Reactions are people's reactions to an issue or a comment. Each one is a
// person and an emoji -- "<login>:<content>" -- and a login means the same
// person on every node, so there's nothing to map. ForgeSync adds and
// removes them as that person (Sudo), exactly as it creates issues as their
// author.
//
// They merge member by member (see set.go), which is what the shape allows:
// a person either reacted or didn't, so two people can never disagree about
// the same reaction and it can't become a conflict. One ForgeSync can't
// write -- a person it can't create on that node, an emoji that node
// doesn't allow -- is tried again next run. Forgejo sends no webhook for
// reactions, so every run reads them.

// reactionSet is the "<login>:<content>" pairs on one issue or comment.
func reactionSet(rs []forgejo.Reaction) map[string]bool {
	out := make(map[string]bool, len(rs))
	for _, r := range rs {
		if r.User.Login != "" && r.Content != "" {
			out[r.User.Login+":"+r.Content] = true
		}
	}
	return out
}

// who splits a reaction into the person and the emoji.
func who(element string) (login, content string) {
	login, content, _ = strings.Cut(element, ":")
	return login, content
}

// reactor adds or removes one reaction on one node, as the person it
// belongs to.
type reactor func(ctx context.Context, node, login, content string, add bool) error

// syncReactions brings one issue's or comment's reactions together and
// returns the new base. ref names it in the log.
func (r *run) syncReactions(ctx context.Context, ref, source, base string, have map[string]map[string]bool, act reactor) string {
	want := setFromValue(base)
	plan := planSet(have, want)
	for _, n := range r.nodes {
		for _, e := range plan.add[n] {
			r.react(ctx, n, ref, source, e, true, have, act)
		}
		for _, e := range plan.remove[n] {
			r.react(ctx, n, ref, source, e, false, have, act)
		}
	}
	return settleSet(have, want)
}

func (r *run) react(ctx context.Context, node, ref, source, element string, add bool, have map[string]map[string]bool, act reactor) {
	login, content := who(element)
	what := "removed"
	if add {
		what = "added"
		if r.s.opts.EnsureUser != nil {
			if err := r.s.opts.EnsureUser(ctx, login, source, node); err != nil {
				r.s.log.Info("issues: a reaction's author can't be created on the node; left for the next run",
					"repository", r.rec.FullName, "node", node, "issue", ref, "author", login, "error", err)
				r.complete = false
				return
			}
		}
	}
	if err := act(ctx, node, login, content, add); err != nil {
		r.s.log.Warn("issues: a reaction couldn't be copied; left for the next run", "repository", r.rec.FullName,
			"node", node, "issue", ref, "reaction", element, "add", add, "error", err)
		r.complete = false
		return
	}
	if have[node] == nil {
		have[node] = map[string]bool{}
	}
	if add {
		have[node][element] = true
	} else {
		delete(have[node], element)
	}
	r.s.log.Info("reaction "+what, "repository", r.rec.FullName, "node", node, "issue", ref,
		"by", login, "reaction", content)
}
