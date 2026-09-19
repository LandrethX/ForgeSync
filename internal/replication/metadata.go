package replication

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// A repository's settings travel like everything else: each single-valued
// one merges against a base, so one new value anywhere wins everywhere and
// two different ones are a conflict for a person with nothing written. Its
// topics are a set, and merge member by member.
//
// Three settings are deliberately not among them.
//
// The default branch is already replication's own business: a replica
// follows the primary's, and a difference the engine can't settle is the
// detector's default_branch_mismatch. Carrying it here as well would mean
// two parts of ForgeSync writing the same field.
//
// Archived would stop replication to any node that took it -- Forgejo
// refuses writes to an archived repository, including ForgeSync's pushes
// -- and ForgeSync's own archive flow owns that flag for the copies of a
// deleted repository.
//
// Fork, mirror and template say how a repository came to be, which isn't
// ForgeSync's to change.
//
// Visibility is carried, but only one way: if any node has the repository
// private, every node gets it. Making something public again is a
// person's decision on each node, because a repository that has been
// public may already have been read, and ForgeSync shouldn't be the one to
// take that step.

// MetadataConflictKind is the conflict kind this pass owns.
const MetadataConflictKind = "repo_metadata"

// metaFields are the settings that travel, in the order they're written.
//
// A toggle is carried only when ForgeSync carries what it promises.
// has_issues and has_pull_requests are, and so is has_wiki once
// replication.wiki is on -- the wiki's own pages travel then, so saying
// the tab is there is true. Projects are boards and cards and actions are
// workflow runs, neither of which ForgeSync copies, so those two toggles
// stay where each node has them.
var metaFields = []string{
	"description", "website", "has_issues", "has_pull_requests",
	"allow_merge_commits", "allow_rebase", "allow_rebase_explicit",
	"allow_squash_merge", "default_merge_style", "delete_branch_after_merge",
}

func metaOf(r forgejo.Repository) map[string]string {
	return map[string]string{
		"description": r.Description, "website": r.Website,
		"has_issues": yes(r.HasIssues), "has_pull_requests": yes(r.HasPullRequests),
		"has_wiki":            yes(r.HasWiki),
		"allow_merge_commits": yes(r.AllowMergeCommits), "allow_rebase": yes(r.AllowRebase),
		"allow_rebase_explicit": yes(r.AllowRebaseExplicit), "allow_squash_merge": yes(r.AllowSquashMerge),
		"default_merge_style": r.DefaultMergeStyle, "delete_branch_after_merge": yes(r.DeleteBranchAfterMerge),
	}
}

func yes(b bool) string { return strconv.FormatBool(b) }

// asField turns a merged value back into what Forgejo takes.
func asField(name, value string) any {
	switch name {
	case "description", "website", "default_merge_style":
		return value
	default:
		return value == "true"
	}
}

// fieldsCarried is metaFields plus the ones that depend on what else is
// turned on.
func (e *Engine) fieldsCarried() []string {
	if e.opts.Wiki {
		return append(append([]string(nil), metaFields...), "has_wiki")
	}
	return metaFields
}

// syncMetadata brings one repository's settings and topics together.
func (e *Engine) syncMetadata(ctx context.Context, rec store.RepositoryRecord, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	at := map[string]forgejo.Repository{}
	topics := map[string]map[string]bool{}
	for _, n := range e.order {
		if !healthy[n] || e.nodes[n].API == nil || !e.hasRepo(rec, n) {
			continue
		}
		r, found, err := e.nodes[n].API.GetRepo(ctx, owner, name)
		if err != nil || !found {
			if err != nil {
				e.log.Warn("metadata: reading a repository failed", "repository", rec.FullName, "node", n, "error", err)
			}
			continue
		}
		list, err := e.nodes[n].API.Topics(ctx, owner, name)
		if err != nil {
			e.log.Warn("metadata: reading the topics failed", "repository", rec.FullName, "node", n, "error", err)
			continue
		}
		at[n] = r
		topics[n] = map[string]bool{}
		for _, t := range list {
			topics[n][t] = true
		}
	}
	if len(at) < 2 {
		return
	}
	was := rec.BaseMetadata
	if was == nil {
		was = map[string]string{}
	}
	// Built fresh, so a setting ForgeSync no longer carries doesn't linger.
	base := map[string]string{}
	var found []store.FoundConflict
	for _, field := range e.fieldsCarried() {
		vals := map[string]string{}
		for n, r := range at {
			vals[n] = metaOf(r)[field]
		}
		value, writes, conflict := mergeField(vals, was[field])
		if conflict {
			found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: MetadataConflictKind, Ref: field,
				Details: map[string]any{"field": field, "values": vals, "primary": rec.PrimaryNode}})
			base[field] = was[field] // the base doesn't move while they differ
			continue
		}
		for _, n := range writes {
			if err := e.nodes[n].API.EditRepoFields(ctx, owner, name, map[string]any{field: asField(field, value)}); err != nil {
				e.log.Warn("metadata: writing a setting failed; left for the next run", "repository", rec.FullName,
					"node", n, "field", field, "error", err)
				continue
			}
			e.log.Info("repository setting written", "repository", rec.FullName, "node", n, "field", field, "value", value)
		}
		base[field] = value
	}
	e.followPrivate(ctx, rec, owner, name, at)
	nowTopics := e.mergeTopics(ctx, rec, owner, name, topics)

	if err := e.store.SetRepositoryMetadata(ctx, rec.ID, base, nowTopics); err != nil {
		e.log.Error("metadata: recording the settings failed", "repository", rec.FullName, "error", err)
	}
	if _, err := e.store.SyncConflicts(ctx, found, []string{rec.ID}, []string{MetadataConflictKind}, e.now()); err != nil {
		e.log.Error("metadata: recording conflicts failed", "repository", rec.FullName, "error", err)
	}
}

// followPrivate makes every node private as soon as any of them is. It
// never goes the other way: a repository that has been public may already
// have been read, and making it public again is a person's decision.
func (e *Engine) followPrivate(ctx context.Context, rec store.RepositoryRecord, owner, name string,
	at map[string]forgejo.Repository) {
	private := false
	for _, r := range at {
		if r.Private {
			private = true
		}
	}
	if !private {
		return
	}
	for _, n := range e.order {
		r, ok := at[n]
		if !ok || r.Private {
			continue
		}
		if err := e.nodes[n].API.EditRepoFields(ctx, owner, name, map[string]any{"private": true}); err != nil {
			e.log.Warn("metadata: making a copy private failed; left for the next run", "repository", rec.FullName,
				"node", n, "error", err)
			continue
		}
		e.log.Info("repository made private, as elsewhere", "repository", rec.FullName, "node", n)
		e.audit(ctx, "repo.made_private", rec.FullName, map[string]any{"repository_id": rec.ID, "node": n})
	}
}

// mergeTopics brings the topics together member by member and returns the
// new base.
func (e *Engine) mergeTopics(ctx context.Context, rec store.RepositoryRecord, owner, name string,
	at map[string]map[string]bool) string {
	base := set.From(rec.BaseTopics)
	plan := set.Decide(at, base)
	changed := map[string]bool{}
	for _, n := range e.order {
		if len(plan.Add[n]) == 0 && len(plan.Remove[n]) == 0 {
			continue
		}
		for _, t := range plan.Remove[n] {
			delete(at[n], t)
		}
		for _, t := range plan.Add[n] {
			at[n][t] = true
		}
		changed[n] = true
	}
	for n := range changed {
		list := make([]string, 0, len(at[n]))
		for t := range at[n] {
			list = append(list, t)
		}
		sort.Strings(list)
		if err := e.nodes[n].API.SetTopics(ctx, owner, name, list); err != nil {
			e.log.Warn("metadata: writing the topics failed; left for the next run", "repository", rec.FullName,
				"node", n, "error", err)
			// Put back what the node still has, so the base doesn't move.
			if now, rerr := e.nodes[n].API.Topics(ctx, owner, name); rerr == nil {
				at[n] = map[string]bool{}
				for _, t := range now {
					at[n][t] = true
				}
			}
			continue
		}
		e.log.Info("repository topics written", "repository", rec.FullName, "node", n, "topics", strings.Join(list, ","))
	}
	return set.Settle(at, base)
}
