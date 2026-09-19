package replication

import (
	"context"
	"errors"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/store"
)

// A wiki is a second git repository, <owner>/<repo>.wiki.git, so it
// replicates the way the repository itself does: the primary's refs are
// fetched and pushed to each replica, only ever creating or
// fast-forwarding, with every push carrying a lease. What Plan decides for
// a branch it decides here too, and a difference it won't settle becomes
// the same kind of conflict, told apart by a ref that says wiki.
//
// One thing is its own. Forgejo doesn't make the wiki's repository until
// someone writes a page, and it won't be pushed into existence: a node
// with no wiki refuses the push and won't let the branch be deleted
// either. So ForgeSync writes one page through the API to bring the
// repository into being, and then replaces exactly that commit with the
// primary's history -- a lease on the commit it just made itself, so a
// wiki someone else has meanwhile written to is never overwritten.

// wikiSeedPage is the page ForgeSync writes to bring a replica's wiki
// repository into being. It lives for one run: the primary's history
// replaces it in the same pass.
const wikiSeedPage = "ForgeSync"

// wikiRemote is a node's wiki repository.
func (e *Engine) wikiRemote(n Node, fullName string) Remote {
	r := e.remote(n, fullName)
	r.URL = strings.TrimSuffix(r.URL, ".git") + ".wiki.git"
	return r
}

// syncWiki brings one repository's wiki from its primary to the replicas.
func (e *Engine) syncWiki(ctx context.Context, rec store.RepositoryRecord, primary Node, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	if primary.API == nil {
		return
	}
	switch has, err := primary.API.HasWiki(ctx, owner, name); {
	case err != nil:
		e.log.Warn("wiki: asking the primary about it failed", "repository", rec.FullName,
			"node", primary.Name, "error", err)
		return
	case !has:
		return // the primary has no wiki; nothing to copy
	}
	pRefs, err := e.git.LsRemote(ctx, e.wikiRemote(primary, rec.FullName))
	if err != nil {
		e.log.Warn("wiki: reading the primary's failed", "repository", rec.FullName, "node", primary.Name, "error", err)
		return
	}
	if len(pRefs) == 0 {
		return
	}
	// Its own bare cache, beside the repository's (the name has to be a
	// plain one, so it's -wiki rather than .wiki).
	dir, err := e.git.Cache(ctx, rec.ID+"-wiki")
	if err != nil {
		e.log.Error("wiki: preparing the cache failed", "repository", rec.FullName, "error", err)
		return
	}
	if err := e.git.Fetch(ctx, dir, "primary", e.wikiRemote(primary, rec.FullName)); err != nil {
		e.log.Warn("wiki: fetching the primary's failed", "repository", rec.FullName, "error", err)
		return
	}

	var found []store.FoundConflict
	for _, n := range e.order {
		if n == primary.Name || !healthy[n] || !e.hasRepo(rec, n) {
			continue
		}
		issues := e.wikiTo(ctx, dir, rec, owner, name, e.nodes[n], pRefs)
		for _, is := range issues {
			found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: string(is.Kind),
				Ref: wikiRef(is.Ref), Details: map[string]any{"wiki": true, "branch": strings.TrimPrefix(is.Ref, "refs/heads/"),
					"primary": primary.Name, "heads": map[string]string{primary.Name: pRefs[is.Ref], n: is.Replica}}})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Ref < found[j].Ref })
	if _, err := e.store.SyncConflicts(ctx, found, []string{rec.ID}, wikiConflictKinds(), e.now()); err != nil {
		e.log.Error("wiki: recording conflicts failed", "repository", rec.FullName, "error", err)
	}
}

// wikiRef names a wiki's branch in a conflict, so it can't be taken for
// the repository's own.
func wikiRef(ref string) string { return "wiki:" + ref }

// wikiConflictKinds are the kinds this pass owns. They're the same kinds
// the repository's own refs use; the ref keeps them apart.
func wikiConflictKinds() []string {
	return []string{string(ReplicaAhead), string(Diverged), string(PrimaryRewrote),
		string(ReplicaChanged), string(ReplicaExtra)}
}

// wikiTo brings one replica's wiki up to date and returns what it won't
// settle by itself.
func (e *Engine) wikiTo(ctx context.Context, dir string, rec store.RepositoryRecord, owner, name string,
	node Node, pRefs Refs) []Issue {
	remote := e.wikiRemote(node, rec.FullName)
	has, err := node.API.HasWiki(ctx, owner, name)
	if err != nil {
		e.log.Warn("wiki: asking the node about it failed", "repository", rec.FullName,
			"node", node.Name, "error", err)
		return nil
	}
	var rRefs Refs
	seeded := false
	if has {
		if rRefs, err = e.git.LsRemote(ctx, remote); err != nil {
			e.log.Warn("wiki: reading the node's failed", "repository", rec.FullName, "node", node.Name, "error", err)
			return nil
		}
	} else {
		// Forgejo makes the wiki's repository when the first page is
		// written, and nothing else will.
		if rRefs, err = e.seedWiki(ctx, owner, name, node, remote); err != nil {
			e.log.Warn("wiki: preparing it on the node failed", "repository", rec.FullName,
				"node", node.Name, "error", err)
			return nil
		}
		seeded = true
	}

	var actions []Action
	var issues []Issue
	if seeded {
		// The only thing there is the page ForgeSync just wrote. Replace it
		// with the primary's history, leased on that very commit, so a wiki
		// someone else has written to meanwhile is never overwritten.
		for ref, sha := range pRefs {
			actions = append(actions, Action{Kind: Reset, Ref: ref, Expected: rRefs[ref], Target: sha})
		}
		sort.Slice(actions, func(i, j int) bool { return actions[i].Ref < actions[j].Ref })
	} else {
		base, err := e.store.WikiRefs(ctx, rec.ID, node.Name)
		if err != nil {
			e.log.Error("wiki: reading what was written failed", "repository", rec.FullName,
				"node", node.Name, "error", err)
			return nil
		}
		actions, issues = Plan(pRefs, rRefs, base, func(a, b string) (bool, bool) {
			return e.git.IsAncestor(ctx, dir, a, b)
		})
	}
	if len(actions) == 0 {
		return issues
	}
	results, err := e.git.Push(ctx, dir, remote, actions)
	if err != nil {
		e.log.Warn("wiki: writing it to the node failed", "repository", rec.FullName, "node", node.Name, "error", err)
		return issues
	}
	next := map[string]string{}
	for ref, sha := range rRefs {
		next[ref] = sha
	}
	for _, a := range actions {
		res := results[a.Ref]
		if !res.OK {
			e.log.Warn("wiki: a branch was refused", "repository", rec.FullName, "node", node.Name,
				"ref", a.Ref, "reason", res.Reason)
			continue
		}
		if a.Kind == Delete {
			delete(next, a.Ref)
			continue
		}
		next[a.Ref] = a.Target
		e.log.Info("wiki updated", "repository", rec.FullName, "node", node.Name, "ref", a.Ref,
			"to", a.Target[:min(8, len(a.Target))], "how", string(a.Kind))
	}
	if err := e.store.SaveWikiRefs(ctx, rec.ID, node.Name, next); err != nil {
		e.log.Error("wiki: recording what was written failed", "repository", rec.FullName,
			"node", node.Name, "error", err)
	}
	return issues
}

// seedWiki writes one page so Forgejo makes the wiki's repository, and
// returns what is then there.
func (e *Engine) seedWiki(ctx context.Context, owner, name string, node Node, remote Remote) (Refs, error) {
	if node.API == nil {
		return nil, errors.New("no API for this node")
	}
	if err := node.API.CreateWikiPage(ctx, owner, name, wikiSeedPage,
		"ForgeSync is about to copy this wiki from the primary.",
		"ForgeSync: preparing the wiki"); err != nil {
		return nil, err
	}
	e.log.Info("wiki prepared on the node", "repository", owner+"/"+name, "node", node.Name)
	return e.git.LsRemote(ctx, remote)
}
