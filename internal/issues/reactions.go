package issues

import (
	"context"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
)

// Reactions are people's reactions to an issue or a comment. Each one is a
// person and an emoji -- "<login>:<content>" -- and a login means the same
// person on every node, so there's nothing to map. ForgeSync adds and
// removes them as that person (Sudo), exactly as it creates issues as their
// author.
//
// They merge per reaction rather than as one value, which is what the shape
// allows: a reaction is either there or not, and the base says which way it
// moved. One that appears anywhere was added and is added everywhere; one
// that is missing anywhere was taken away and is taken away everywhere. Two
// people can't disagree about the same reaction, so unlike a title or a set
// of labels this can't become a conflict.
//
// A reaction only enters the base once every node has it, and only leaves
// once no node has it. So a copy ForgeSync couldn't write -- a person it
// can't create on that node, an emoji that node doesn't allow -- stays
// half-done and is tried again next run, instead of its absence being read
// as someone's change and copied everywhere. Forgejo sends no webhook for
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

// reactionValue is a set as the base records it.
func reactionValue(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func reactionsFromValue(value string) map[string]bool {
	out := map[string]bool{}
	for _, e := range split(value) {
		out[e] = true
	}
	return out
}

// who splits a reaction into the person and the emoji.
func who(element string) (login, content string) {
	login, content, _ = strings.Cut(element, ":")
	return login, content
}

// reactionPlan is what each node needs for one issue or comment.
type reactionPlan struct {
	add    map[string][]string // node -> reactions to add there
	remove map[string][]string // node -> reactions to take away there
}

// planReactions decides every reaction across the nodes that have a copy.
// have maps a node to what it has; base is what they last all agreed on.
func planReactions(have map[string]map[string]bool, base map[string]bool) reactionPlan {
	p := reactionPlan{add: map[string][]string{}, remove: map[string][]string{}}
	seen := map[string]bool{}
	for e := range base {
		seen[e] = true
	}
	for _, set := range have {
		for e := range set {
			seen[e] = true
		}
	}
	elements := make([]string, 0, len(seen))
	for e := range seen {
		elements = append(elements, e)
	}
	sort.Strings(elements) // a run's writes in a settled order
	for _, e := range elements {
		var with, without []string
		for n, set := range have {
			if set[e] {
				with = append(with, n)
			} else {
				without = append(without, n)
			}
		}
		switch {
		case base[e] && len(without) > 0:
			// It was there for everyone and is gone somewhere: taken away.
			for _, n := range with {
				p.remove[n] = append(p.remove[n], e)
			}
		case !base[e] && len(with) > 0:
			// It was nowhere and is somewhere: added.
			for _, n := range without {
				p.add[n] = append(p.add[n], e)
			}
		}
	}
	for n := range p.add {
		sort.Strings(p.add[n])
	}
	for n := range p.remove {
		sort.Strings(p.remove[n])
	}
	return p
}

// settle is the base after a round of writes: a reaction counts as agreed
// only once every node has it, and as gone only once none has. Anything
// half-done keeps its old base and is tried again next run.
func settle(have map[string]map[string]bool, base map[string]bool) string {
	seen := map[string]bool{}
	for e := range base {
		seen[e] = true
	}
	for _, set := range have {
		for e := range set {
			seen[e] = true
		}
	}
	out := map[string]bool{}
	for e := range seen {
		all, none := true, true
		for _, set := range have {
			if set[e] {
				none = false
			} else {
				all = false
			}
		}
		switch {
		case len(have) > 0 && all:
			out[e] = true
		case len(have) > 0 && none:
			// gone everywhere: out of the base
		case base[e]:
			out[e] = true // half-done: the base stays where it was
		}
	}
	return reactionValue(out)
}

// reactor adds or removes one reaction on one node, as the person it
// belongs to.
type reactor func(ctx context.Context, node, login, content string, add bool) error

// syncReactions brings one issue's or comment's reactions together and
// returns the new base. ref names it in the log.
func (r *run) syncReactions(ctx context.Context, ref, source, base string, have map[string]map[string]bool, act reactor) string {
	want := reactionsFromValue(base)
	plan := planReactions(have, want)
	for _, n := range r.nodes {
		for _, e := range plan.add[n] {
			r.react(ctx, n, ref, source, e, true, have, act)
		}
		for _, e := range plan.remove[n] {
			r.react(ctx, n, ref, source, e, false, have, act)
		}
	}
	return settle(have, want)
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
