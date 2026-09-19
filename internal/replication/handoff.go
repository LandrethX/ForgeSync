package replication

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// Resolving conflicts.
//
// Fixes that lose nothing run on their own (autoFix): a replica that is only
// ahead of the primary has its commits taken over by the primary, a ref that
// exists only on a replica is created on the primary, and a replica's default
// branch is set to the primary's. From the primary they replicate as usual.
//
// A diverged branch (both sides have commits the other lacks, or history was
// rewritten on the primary) goes to the repository's owner: the replicas'
// version is pushed to the primary as refs/heads/forgesync/conflict/<node>/<branch>
// and a pull request is opened from it into the branch, assigned to the
// owner (for an organization: its owners). The owner decides in Forgejo:
//
//   - Merged: the primary now contains the replicas' commits, so the replicas
//     are simply behind and fast-forward. The hand-off branch is deleted.
//   - Closed without merging: keep the primary's version. ForgeSync resets the
//     branch on those replicas (only if they're still at the handed-off
//     commit) and keeps the hand-off branch as a backup for BackupFor.
//
// Hand-off branches live on the primary only; they're never replicated.

const handoffPrefix = "refs/heads/forgesync/conflict/"

// splitHandoffRefs separates hand-off branches from the refs to replicate.
func splitHandoffRefs(all Refs) (refs, handoff Refs) {
	refs, handoff = Refs{}, Refs{}
	for ref, sha := range all {
		if strings.HasPrefix(ref, handoffPrefix) {
			handoff[ref] = sha
		} else {
			refs[ref] = sha
		}
	}
	return refs, handoff
}

// handOffKind reports whether an issue goes to the owner as a pull request.
func handOffKind(is Issue) bool {
	return (is.Kind == Diverged || is.Kind == PrimaryRewrote) && strings.HasPrefix(is.Ref, "refs/heads/") && is.Replica != ""
}

type divergence struct {
	node  string
	issue Issue
}

// handoffInfo is what a conflict's details say about its hand-offs.
type handoffInfo struct {
	PRNumber int64    `json:"pr_number"`
	PRURL    string   `json:"pr_url"`
	Branch   string   `json:"branch"`
	Nodes    []string `json:"nodes"`
	State    string   `json:"state"`
}

func handoffsFor(hs []store.Handoff, ref string, heads map[string]string) []handoffInfo {
	var out []handoffInfo
	for _, h := range hs {
		if h.Ref != ref || h.State != "open" {
			continue
		}
		for _, sha := range heads {
			if sha == h.SHA {
				out = append(out, handoffInfo{PRNumber: h.PRNumber, PRURL: h.PRURL,
					Branch: strings.TrimPrefix(h.Branch, "refs/heads/"), Nodes: h.Nodes, State: h.State})
				break
			}
		}
	}
	return out
}

// autoFix applies the fixes that lose nothing to one replica's issues, by
// updating the primary. It returns the issues left and how many it fixed.
func (e *Engine) autoFix(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node, node string,
	pRefs Refs, issues []Issue) ([]Issue, int) {

	var actions []Action
	for _, is := range issues {
		switch is.Kind {
		case ReplicaAhead: // the primary is an ancestor: fast-forward it
			actions = append(actions, Action{Kind: FastForward, Ref: is.Ref, Expected: is.Primary, Target: is.Replica})
		case ReplicaExtra: // only on the replica: create it on the primary
			actions = append(actions, Action{Kind: Create, Ref: is.Ref, Expected: "", Target: is.Replica})
		}
	}
	if len(actions) == 0 {
		return issues, 0
	}
	// Each push is a compare-and-swap on the primary's value, so a change
	// there in the meantime (or another replica's fix to the same ref) makes
	// it fail and the issue stays.
	results, err := e.git.Push(ctx, dir, e.remote(primary, rec.FullName), actions)
	if err != nil {
		e.log.Warn("taking a replica's refs to the primary failed", "repository", rec.FullName, "node", node, "error", err)
		return issues, 0
	}
	fixed := map[string]bool{}
	for _, a := range actions {
		if r := results[a.Ref]; r.OK {
			fixed[a.Ref] = true
			pRefs[a.Ref] = a.Target
			e.log.Info("primary took over a replica's ref", "repository", rec.FullName, "node", node, "ref", a.Ref)
			e.audit(ctx, "conflict.auto_fixed", rec.FullName, map[string]any{
				"ref": a.Ref, "node": node, "primary": primary.Name, "fix": string(a.Kind), "sha": a.Target})
		}
	}
	left := issues[:0:0]
	for _, is := range issues {
		if !fixed[is.Ref] {
			left = append(left, is)
		}
	}
	return left, len(fixed)
}

// fixDefaultBranches sets each replica's default branch to the primary's,
// once that branch exists there.
func (e *Engine) fixDefaultBranches(ctx context.Context, rec store.RepositoryRecord, primary Node, healthy map[string]bool) {
	want := ""
	for _, r := range rec.Replicas {
		if r.Node == primary.Name && r.Present {
			want = r.DefaultBranch
		}
	}
	if want == "" {
		return
	}
	owner, name, _ := strings.Cut(rec.FullName, "/")
	for _, r := range rec.Replicas {
		n, ok := e.nodes[r.Node]
		if !ok || r.Node == primary.Name || !r.Present || r.Empty || r.Mirror || r.DefaultBranch == want || !healthy[r.Node] || n.API == nil {
			continue
		}
		if err := n.API.SetDefaultBranch(ctx, owner, name, want); err != nil {
			e.log.Warn("setting the default branch failed", "repository", rec.FullName, "node", r.Node, "error", err)
			continue
		}
		e.log.Info("default branch set to the primary's", "repository", rec.FullName, "node", r.Node, "branch", want)
		e.audit(ctx, "conflict.auto_fixed", rec.FullName, map[string]any{
			"node": r.Node, "primary": primary.Name, "fix": "default_branch", "from": r.DefaultBranch, "to": want})
	}
}

// applyDecisions acts on owners' decisions in open hand-offs and removes
// expired backups. It returns the hand-offs still active.
func (e *Engine) applyDecisions(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node,
	pRefs, handoffRefs Refs, hs []store.Handoff, healthy map[string]bool) []store.Handoff {

	if primary.API == nil {
		return hs
	}
	owner, name, _ := strings.Cut(rec.FullName, "/")
	now := e.now().UTC()
	var active []store.Handoff
	for _, h := range hs {
		switch h.State {
		case "open":
			pr, found, err := primary.API.GetPullRequest(ctx, owner, name, h.PRNumber)
			switch {
			case err != nil:
				e.log.Warn("reading a hand-off pull request failed", "repository", rec.FullName, "pr", h.PRNumber, "error", err)
			case !found:
				h.State, h.DecidedAt = "gone", &now
				e.saveHandoff(ctx, h)
				e.audit(ctx, "conflict.handoff_gone", rec.FullName, map[string]any{"ref": h.Ref, "pr_number": h.PRNumber})
				continue // it's handed off again below if still diverged
			case pr.Merged:
				h.State, h.DecidedAt = "merged", &now
				e.deleteHandoffBranch(ctx, dir, rec, primary, handoffRefs, h)
				e.saveHandoff(ctx, h)
				e.audit(ctx, "conflict.owner_merged", rec.FullName, map[string]any{
					"ref": h.Ref, "pr_number": h.PRNumber, "nodes": h.Nodes})
				continue
			case pr.State == "closed":
				if h.DecidedAt == nil {
					h.DecidedAt = &now
				}
				if e.resetReplicas(ctx, dir, rec, primary, pRefs, &h, healthy) {
					until := now.Add(e.opts.BackupFor)
					h.State, h.BackupUntil = "kept_primary", &until
					e.audit(ctx, "conflict.owner_kept_primary", rec.FullName, map[string]any{
						"ref": h.Ref, "pr_number": h.PRNumber, "backup": strings.TrimPrefix(h.Branch, "refs/heads/"),
						"backup_until": until})
					e.comment(ctx, primary, owner, name, h.PRNumber, fmt.Sprintf(
						"ForgeSync reset `%s` on the other sites to this site's version, as you chose by closing this pull request. "+
							"Their commits stay on the branch `%s` until %s, then ForgeSync deletes it.",
						strings.TrimPrefix(h.Ref, "refs/heads/"), strings.TrimPrefix(h.Branch, "refs/heads/"), until.Format("2006-01-02")))
				}
				e.saveHandoff(ctx, h)
			}
		case "kept_primary":
			if h.BackupUntil != nil && now.After(*h.BackupUntil) {
				e.deleteHandoffBranch(ctx, dir, rec, primary, handoffRefs, h)
				h.State = "expired"
				e.saveHandoff(ctx, h)
				e.audit(ctx, "conflict.backup_deleted", rec.FullName, map[string]any{
					"ref": h.Ref, "branch": strings.TrimPrefix(h.Branch, "refs/heads/")})
				continue
			}
		}
		active = append(active, h)
	}
	return active
}

// resetReplicas sets the branch on each replica still waiting in h to the
// primary's version, but only where it's still the handed-off commit. It
// removes the replicas it's done with from h.Nodes and reports whether none
// are left.
func (e *Engine) resetReplicas(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node, pRefs Refs,
	h *store.Handoff, healthy map[string]bool) bool {

	target, onPrimary := pRefs[h.Ref]
	left := []string{} // not nil: it's stored as a list, never as nothing
	for _, node := range h.Nodes {
		n, ok := e.nodes[node]
		if !ok {
			continue // no longer configured
		}
		if !healthy[node] {
			left = append(left, node)
			continue
		}
		a := Action{Kind: Reset, Ref: h.Ref, Expected: h.SHA, Target: target}
		if !onPrimary { // the owner deleted the branch on the primary
			a = Action{Kind: Delete, Ref: h.Ref, Expected: h.SHA}
		}
		// Forgejo won't let anyone rewrite or delete a protected branch, so
		// ForgeSync lifts its own guard for this and puts it straight back.
		var results map[string]PushResult
		err := e.withoutGuard(ctx, rec, e.nodes[node], func() (err error) {
			results, err = e.git.Push(ctx, dir, e.remote(n, rec.FullName), []Action{a})
			return err
		})
		res := results[h.Ref]
		switch {
		case err != nil:
			e.log.Warn("resetting a replica failed", "repository", rec.FullName, "node", node, "error", err)
			left = append(left, node)
		case res.OK:
			e.log.Info("replica reset to the primary's version", "repository", rec.FullName, "node", node, "ref", h.Ref)
			e.audit(ctx, "conflict.replica_reset", rec.FullName, map[string]any{
				"ref": h.Ref, "node": node, "from": h.SHA, "to": target, "pr_number": h.PRNumber})
		case strings.Contains(res.Reason, "stale info"):
			// Someone pushed on the replica since; that's a new divergence,
			// handed off on its own.
			e.log.Info("replica changed since the hand-off; not reset", "repository", rec.FullName, "node", node, "ref", h.Ref)
		default:
			// E.g. a protected branch, which refuses non-fast-forward pushes
			// even from ForgeSync.
			e.log.Warn("replica refused the reset", "repository", rec.FullName, "node", node, "ref", h.Ref, "reason", res.Reason)
			left = append(left, node)
		}
	}
	h.Nodes = left
	return len(left) == 0
}

// handOff opens a pull request for each diverged branch version that doesn't
// have one yet. It returns the active hand-offs, including new ones.
func (e *Engine) handOff(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node,
	pRefs, handoffRefs Refs, hs []store.Handoff, found []divergence) []store.Handoff {

	if primary.API == nil || len(found) == 0 {
		return hs
	}
	// One hand-off per branch and replica head: replicas with the same
	// version share a pull request.
	type version struct{ ref, sha string }
	groups := map[version][]string{}
	var order []version
	for _, d := range found {
		v := version{d.issue.Ref, d.issue.Replica}
		if groups[v] == nil {
			order = append(order, v)
		}
		groups[v] = append(groups[v], d.node)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].ref+order[i].sha < order[j].ref+order[j].sha })

	owner, name, _ := strings.Cut(rec.FullName, "/")
	for _, v := range order {
		nodes := groups[v]
		sort.Strings(nodes)
		if i := slices.IndexFunc(hs, func(h store.Handoff) bool { return h.State == "open" && h.Ref == v.ref && h.SHA == v.sha }); i >= 0 {
			if h := &hs[i]; !sameSet(h.Nodes, nodes) {
				h.Nodes = union2(h.Nodes, nodes)
				e.saveHandoff(ctx, *h)
			}
			continue
		}
		if i := e.extendHandoff(ctx, dir, rec, primary, handoffRefs, hs, v.ref, v.sha, nodes); i >= 0 {
			continue
		}
		h, err := e.openHandoff(ctx, dir, rec, primary, owner, name, pRefs, handoffRefs, v.ref, v.sha, nodes)
		if err != nil {
			e.log.Warn("handing a diverged branch to the owner failed", "repository", rec.FullName, "ref", v.ref, "error", err)
			continue
		}
		hs = append(hs, h)
	}
	return hs
}

// extendHandoff moves an open hand-off along when the replicas carried on
// committing (their new head contains the handed-off one) and the owner
// hasn't touched the hand-off branch. It returns the hand-off's index, or -1.
func (e *Engine) extendHandoff(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node,
	handoffRefs Refs, hs []store.Handoff, ref, sha string, nodes []string) int {

	for i := range hs {
		h := &hs[i]
		if h.State != "open" || h.Ref != ref || handoffRefs[h.Branch] != h.SHA || !sameSet(h.Nodes, nodes) {
			continue
		}
		if is, ok := e.git.IsAncestor(ctx, dir, h.SHA, sha); !ok || !is {
			continue
		}
		res, err := e.git.Push(ctx, dir, e.remote(primary, rec.FullName),
			[]Action{{Kind: FastForward, Ref: h.Branch, Expected: h.SHA, Target: sha}})
		if err != nil || !res[h.Branch].OK {
			return -1
		}
		h.SHA = sha
		e.saveHandoff(ctx, *h)
		owner, name, _ := strings.Cut(rec.FullName, "/")
		e.comment(ctx, primary, owner, name, h.PRNumber, fmt.Sprintf(
			"New commits from %s were added to this pull request (now at `%s`).", strings.Join(nodes, ", "), short(sha)))
		return i
	}
	return -1
}

func (e *Engine) openHandoff(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node, owner, name string,
	pRefs, handoffRefs Refs, ref, sha string, nodes []string) (store.Handoff, error) {

	branch := strings.TrimPrefix(ref, "refs/heads/")
	full := handoffPrefix + nodes[0] + "/" + branch
	for n := 2; handoffRefs[full] != ""; n++ {
		full = fmt.Sprintf("%s%s/%s-%d", handoffPrefix, nodes[0], branch, n)
	}
	remote := e.remote(primary, rec.FullName)
	res, err := e.git.Push(ctx, dir, remote, []Action{{Kind: Create, Ref: full, Target: sha}})
	if err != nil {
		return store.Handoff{}, err
	}
	if r := res[full]; !r.OK {
		return store.Handoff{}, fmt.Errorf("%s refused %s: %s", primary.Name, full, r.Reason)
	}
	handoffRefs[full] = sha

	assignees, err := e.decisionMakers(ctx, primary, owner)
	if err != nil {
		e.log.Warn("finding who decides failed; opening the pull request unassigned", "repository", rec.FullName, "error", err)
	}
	head := strings.TrimPrefix(full, "refs/heads/")
	unrelated := false
	if pSHA := pRefs[ref]; pSHA != "" {
		if base, err := e.git.MergeBase(ctx, dir, pSHA, sha); err == nil && base == "" {
			unrelated = true
		}
	}
	opt := forgejo.CreatePullRequestOption{
		Head: head, Base: branch, Assignees: assignees,
		Title: fmt.Sprintf("ForgeSync: %s has diverged on %s", branch, strings.Join(nodes, ", ")),
		Body:  handoffBody(primary.Name, branch, head, sha, nodes, assignees, unrelated, e.opts.BackupFor),
	}
	pr, err := primary.API.CreatePullRequest(ctx, owner, name, opt)
	if err != nil && len(assignees) > 0 {
		// Assignees need write access; the body still mentions them.
		opt.Assignees = nil
		pr, err = primary.API.CreatePullRequest(ctx, owner, name, opt)
	}
	if err != nil {
		// Don't leave a branch without its pull request. If even that
		// fails, say so: the branch is then on the primary with nothing
		// pointing at it, and the next run will try the hand-off again.
		if _, perr := e.git.Push(ctx, dir, remote, []Action{{Kind: Delete, Ref: full, Expected: sha}}); perr != nil {
			e.log.Warn("couldn't remove the hand-off branch after the pull request failed",
				"repository", rec.FullName, "ref", full, "error", perr)
		}
		delete(handoffRefs, full)
		return store.Handoff{}, err
	}
	h := store.Handoff{RepositoryID: rec.ID, Ref: ref, SHA: sha, PrimaryNode: primary.Name, Nodes: nodes,
		Branch: full, PRNumber: pr.Number, PRURL: pr.HTMLURL, State: "open", OpenedAt: e.now().UTC()}
	if h.ID, err = e.store.SaveHandoff(ctx, h); err != nil {
		return store.Handoff{}, err
	}
	e.log.Info("diverged branch handed to the owner", "repository", rec.FullName, "ref", ref, "nodes", nodes, "pr", pr.HTMLURL)
	e.audit(ctx, "conflict.handed_off", rec.FullName, map[string]any{
		"ref": ref, "nodes": nodes, "pr_number": pr.Number, "pr_url": pr.HTMLURL, "assignees": assignees})
	return h, nil
}

// decisionMakers are who a hand-off is assigned to: the owning user, or an
// organization's owners.
func (e *Engine) decisionMakers(ctx context.Context, primary Node, owner string) ([]string, error) {
	isOrg, err := primary.API.IsOrg(ctx, owner)
	if err != nil {
		return nil, err
	}
	if !isOrg {
		return []string{owner}, nil
	}
	return primary.API.OrgOwners(ctx, owner)
}

func handoffBody(primary, branch, head, sha string, nodes, assignees []string, unrelated bool, backup time.Duration) string {
	var b strings.Builder
	if len(assignees) > 0 {
		fmt.Fprintf(&b, "@%s, ForgeSync needs your decision.\n\n", strings.Join(assignees, " @"))
	}
	fmt.Fprintf(&b, "This repository is kept in sync across sites, with **%s** as its primary. "+
		"The branch `%s` on **%s** has commits that aren't in `%s` here, and `%s` here has commits it doesn't have, "+
		"so ForgeSync can't copy it either way without losing work.\n\n", primary, branch, strings.Join(nodes, ", "), branch, branch)
	fmt.Fprintf(&b, "This pull request holds their version (`%s`) on the branch `%s`. Please choose:\n\n", short(sha), head)
	if unrelated {
		b.WriteString("- **Keep both:** not possible with a merge here: the two versions share no history, so Forgejo can't merge them. " +
			"If both are needed, combine them by hand on `" + branch + "`, then close this pull request.\n")
	} else {
		fmt.Fprintf(&b, "- **Keep both:** merge this pull request. If Forgejo reports conflicts, merge `%s` into `%s` locally, "+
			"resolve them, push `%s`, and then merge or close this pull request.\n", head, branch, branch)
	}
	fmt.Fprintf(&b, "- **Keep only this site's version:** close this pull request without merging. ForgeSync then resets `%s` on %s "+
		"to this site's version. Their commits stay on `%s` for %d days, then ForgeSync deletes that branch.\n\n",
		branch, strings.Join(nodes, ", "), head, int(backup.Hours()/24))
	b.WriteString("Until you decide, those sites keep their version of `" + branch + "` and it isn't replicated. " +
		"Other branches keep replicating as usual.")
	return b.String()
}

func (e *Engine) deleteHandoffBranch(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node, handoffRefs Refs, h store.Handoff) {
	cur, ok := handoffRefs[h.Branch]
	if !ok {
		return
	}
	res, err := e.git.Push(ctx, dir, e.remote(primary, rec.FullName), []Action{{Kind: Delete, Ref: h.Branch, Expected: cur}})
	if err != nil || !res[h.Branch].OK {
		e.log.Warn("deleting a hand-off branch failed", "repository", rec.FullName, "branch", h.Branch, "error", err)
		return
	}
	delete(handoffRefs, h.Branch)
}

func (e *Engine) comment(ctx context.Context, primary Node, owner, name string, pr int64, body string) {
	if err := primary.API.Comment(ctx, owner, name, pr, body); err != nil {
		e.log.Warn("commenting on a hand-off pull request failed", "repository", owner+"/"+name, "pr", pr, "error", err)
	}
}

func (e *Engine) saveHandoff(ctx context.Context, h store.Handoff) {
	if err := e.store.UpdateHandoff(ctx, h); err != nil {
		e.log.Error("saving a hand-off failed", "id", h.ID, "error", err)
	}
}

func (e *Engine) audit(ctx context.Context, action, target string, details map[string]any) {
	if err := e.store.Audit(ctx, "forgesync", action, target, details); err != nil {
		e.log.Error("writing audit log failed", "action", action, "error", err)
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func sameSet(a, b []string) bool {
	return len(union2(a, b)) == len(a) && len(a) == len(b)
}

func union2(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
