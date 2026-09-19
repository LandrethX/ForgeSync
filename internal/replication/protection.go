package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// Branch protection is two separate things here.
//
// The first is ForgeSync's own guard on the replicas, which is the answer
// to Phase 0's p02: a replica is a copy, and a push to it is work that
// only conflicts with the primary. The guard is a rule over every branch
// (**) that lets nobody push -- except site admins, which is how the
// service account still replicates. Phase 0 found the owner can delete a
// protection rule, so ForgeSync puts the guard back whenever it's gone or
// has been changed. It never goes on the primary, and it never takes part
// in the merge below.
//
// Forgejo won't let even a site admin force-push to a protected branch or
// delete one, and ForgeSync needs both: a hand-off reset rewrites a
// replica's branch, and a branch deleted on the primary is deleted on the
// replicas. So it lifts its own guard for those writes and puts it back
// straight after (withoutGuard). The gap is milliseconds on a replica.
//
// The second is the owner's own rules, which travel like everything else:
// each is one member, "<rule>:<digest of its settings>", merged with
// internal/set. A rule added anywhere is added everywhere, one removed
// anywhere is removed everywhere, and changing a rule's settings is one
// member going and another arriving -- written as an edit rather than a
// removal and a fresh rule, so a branch is never briefly unprotected.

// guardRule covers every branch. Forgejo matches it as a glob.
const guardRule = "**"

// guardWants is the guard as it should be: pushing is whitelisted, and the
// only name on the whitelist is the service account. Being a site admin is
// not enough -- apply_to_admins off exempts admins from the rules that
// need one, not from the push whitelist -- so the service account has to
// be named or ForgeSync locks itself out.
//
// Forgejo takes that name but won't give it back: the whitelist reads as
// empty however it was set. So guardIsRight can only check what can be
// read, and a whitelist someone has emptied shows up as pushes being
// refused, which reassertGuard then repairs.
func guardWants(serviceUser string) forgejo.BranchProtection {
	return forgejo.BranchProtection{RuleName: guardRule, EnablePush: true, EnablePushWhitelist: true,
		PushWhitelistUsernames: []string{serviceUser}, ApplyToAdmins: false}
}

// guardIsRight reports that a rule is the guard as far as can be seen.
func guardIsRight(r forgejo.BranchProtection) bool {
	return r.EnablePush && r.EnablePushWhitelist && !r.ApplyToAdmins
}

// reassertGuard writes the guard's settings again, whitelist and all. It's
// what ForgeSync does when a node refuses its push because of protection:
// the whitelist can't be read, so being refused is the only way to find
// out that it's gone.
func (e *Engine) reassertGuard(ctx context.Context, rec store.RepositoryRecord, n Node) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok || n.API == nil {
		return
	}
	if err := n.API.EditBranchProtection(ctx, owner, name, guardRule, guardWants(n.User)); err != nil {
		e.log.Warn("protection: the guard couldn't be written again", "repository", rec.FullName,
			"node", n.Name, "error", err)
		return
	}
	e.log.Info("replica guard written again after a push it refused", "repository", rec.FullName, "node", n.Name)
	e.audit(ctx, "repo.replica_guard_restored", rec.FullName, map[string]any{"repository_id": rec.ID, "node": n.Name})
}

// protectReplicas keeps the guard on every replica and off the primary.
func (e *Engine) protectReplicas(ctx context.Context, rec store.RepositoryRecord, primary Node, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	for _, n := range e.order {
		if !healthy[n] || e.nodes[n].API == nil || !e.hasRepo(rec, n) {
			continue
		}
		rules, err := e.nodes[n].API.BranchProtections(ctx, owner, name)
		if err != nil {
			e.log.Warn("protection: reading the rules failed", "repository", rec.FullName, "node", n, "error", err)
			continue
		}
		var guard *forgejo.BranchProtection
		for i := range rules {
			if rules[i].RuleName == guardRule {
				guard = &rules[i]
			}
		}
		if n == primary.Name {
			// The primary is where people work; the guard has no business
			// there, even if this repository's primary has moved.
			if guard != nil {
				if err := e.nodes[n].API.DeleteBranchProtection(ctx, owner, name, guardRule); err != nil {
					e.log.Warn("protection: lifting the guard on the primary failed", "repository", rec.FullName,
						"node", n, "error", err)
					continue
				}
				e.log.Info("guard lifted on the primary", "repository", rec.FullName, "node", n)
			}
			continue
		}
		switch {
		case guard == nil:
			if _, err := e.nodes[n].API.CreateBranchProtection(ctx, owner, name, guardWants(e.nodes[n].User)); err != nil {
				e.log.Warn("protection: guarding the replica failed", "repository", rec.FullName, "node", n, "error", err)
				continue
			}
			e.log.Info("replica guarded", "repository", rec.FullName, "node", n, "rule", guardRule)
			e.audit(ctx, "repo.replica_guarded", rec.FullName, map[string]any{"repository_id": rec.ID, "node": n})
		case !guardIsRight(*guard):
			// Someone changed it. Put it back the way it was.
			if err := e.nodes[n].API.EditBranchProtection(ctx, owner, name, guardRule, guardWants(e.nodes[n].User)); err != nil {
				e.log.Warn("protection: restoring the guard failed", "repository", rec.FullName, "node", n, "error", err)
				continue
			}
			e.log.Info("replica guard restored", "repository", rec.FullName, "node", n)
			e.audit(ctx, "repo.replica_guard_restored", rec.FullName, map[string]any{"repository_id": rec.ID, "node": n})
		}
	}
}

// withoutGuard lifts ForgeSync's own guard on a node, runs write, and puts
// it back. Forgejo won't let anyone rewrite or delete a protected branch,
// not even a site admin, so this is how a hand-off reset and a deletion
// get through. If the guard isn't there, nothing is touched.
func (e *Engine) withoutGuard(ctx context.Context, rec store.RepositoryRecord, n Node, write func() error) error {
	if !e.opts.ProtectReplicas || n.API == nil {
		return write()
	}
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return write()
	}
	rules, err := n.API.BranchProtections(ctx, owner, name)
	if err != nil {
		return write() // can't tell; the write will say if it's blocked
	}
	var had *forgejo.BranchProtection
	for i := range rules {
		if rules[i].RuleName == guardRule {
			had = &rules[i]
		}
	}
	if had == nil {
		return write()
	}
	if err := n.API.DeleteBranchProtection(ctx, owner, name, guardRule); err != nil {
		return fmt.Errorf("lifting the guard on %s: %w", n.Name, err)
	}
	defer func() {
		if _, err := n.API.CreateBranchProtection(ctx, owner, name, guardWants(n.User)); err != nil {
			e.log.Error("protection: the guard couldn't be put back; the next run restores it",
				"repository", rec.FullName, "node", n.Name, "error", err)
		}
	}()
	return write()
}

// ruleMember identifies one of the owner's rules: its name and a digest of
// the settings that travel with it.
func ruleMember(r forgejo.BranchProtection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v|%v|%v|%v|%v|%d|%v|%v|%v|%v|%v|%v|%q|%q|%v",
		r.EnablePush, r.EnablePushWhitelist, sorted(r.PushWhitelistUsernames), sorted(r.PushWhitelistTeams),
		r.PushWhitelistDeployKeys, r.RequiredApprovals, r.EnableMergeWhitelist, sorted(r.MergeWhitelistUsernames),
		r.EnableStatusCheck, sorted(r.StatusCheckContexts), r.BlockOnRejectedReviews, r.BlockOnOutdatedBranch,
		r.ProtectedFilePatterns, r.UnprotectedFilePatterns, r.ApplyToAdmins)
	sum := sha256.Sum256([]byte(b.String()))
	return r.RuleName + ":" + hex.EncodeToString(sum[:])[:12]
}

func sorted(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

// ruleNameOf is the branch pattern in a member.
func ruleNameOf(member string) string {
	i := strings.LastIndex(member, ":")
	if i < 0 {
		return member
	}
	return member[:i]
}

// syncProtection brings the owner's own rules together across the nodes.
func (e *Engine) syncProtection(ctx context.Context, rec store.RepositoryRecord, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	at := map[string]map[string]forgejo.BranchProtection{}
	have := map[string]map[string]bool{}
	for _, n := range e.order {
		if !healthy[n] || e.nodes[n].API == nil || !e.hasRepo(rec, n) {
			continue
		}
		rules, err := e.nodes[n].API.BranchProtections(ctx, owner, name)
		if err != nil {
			e.log.Warn("protection: reading the rules failed", "repository", rec.FullName, "node", n, "error", err)
			continue
		}
		at[n], have[n] = map[string]forgejo.BranchProtection{}, map[string]bool{}
		for _, r := range rules {
			if r.RuleName == guardRule {
				continue // ForgeSync's own; not the owner's to spread
			}
			at[n][ruleMember(r)], have[n][ruleMember(r)] = r, true
		}
	}
	if len(have) < 2 {
		return
	}
	base := set.From(rec.BaseProtection)
	plan := set.Decide(have, base)
	for _, n := range e.order {
		if _, taking := have[n]; !taking {
			continue
		}
		editing := map[string]bool{}
		for _, m := range plan.Add[n] {
			editing[ruleNameOf(m)] = true
		}
		for _, m := range plan.Remove[n] {
			// A rule whose settings are only changing is edited below, so
			// its branches are never briefly unprotected.
			if editing[ruleNameOf(m)] {
				delete(have[n], m)
				continue
			}
			if err := e.nodes[n].API.DeleteBranchProtection(ctx, owner, name, ruleNameOf(m)); err != nil {
				e.log.Warn("protection: removing a rule failed; left for the next run", "repository", rec.FullName,
					"node", n, "rule", ruleNameOf(m), "error", err)
				continue
			}
			delete(have[n], m)
			e.log.Info("protection rule removed", "repository", rec.FullName, "node", n, "rule", ruleNameOf(m))
			e.audit(ctx, "repo.protection_removed", rec.FullName,
				map[string]any{"repository_id": rec.ID, "node": n, "rule": ruleNameOf(m)})
		}
		for _, m := range plan.Add[n] {
			want, from, found := ruleFrom(e.order, at, m)
			if !found {
				continue
			}
			var err error
			if _, had := at[n][m]; !had && editing[ruleNameOf(m)] && hasRuleNamed(at[n], ruleNameOf(m)) {
				err = e.nodes[n].API.EditBranchProtection(ctx, owner, name, ruleNameOf(m), want)
			} else {
				_, err = e.nodes[n].API.CreateBranchProtection(ctx, owner, name, want)
			}
			if err != nil {
				e.log.Warn("protection: writing a rule failed; left for the next run", "repository", rec.FullName,
					"node", n, "rule", ruleNameOf(m), "from", from, "error", err)
				continue
			}
			have[n][m] = true
			e.log.Info("protection rule written", "repository", rec.FullName, "node", n, "rule", ruleNameOf(m), "from", from)
			e.audit(ctx, "repo.protection_written", rec.FullName,
				map[string]any{"repository_id": rec.ID, "node": n, "rule": ruleNameOf(m)})
		}
	}
	if now := set.Settle(have, base); now != rec.BaseProtection {
		if err := e.store.SetRepositoryProtection(ctx, rec.ID, now); err != nil {
			e.log.Error("protection: recording the rules failed", "repository", rec.FullName, "error", err)
		}
	}
}

// ruleFrom is a rule as some node has it, to write elsewhere.
func ruleFrom(order []string, at map[string]map[string]forgejo.BranchProtection, member string) (forgejo.BranchProtection, string, bool) {
	for _, n := range order {
		if r, ok := at[n][member]; ok {
			return r, n, true
		}
	}
	return forgejo.BranchProtection{}, "", false
}

func hasRuleNamed(rules map[string]forgejo.BranchProtection, name string) bool {
	for _, r := range rules {
		if r.RuleName == name {
			return true
		}
	}
	return false
}
