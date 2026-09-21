package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// What ForgeSync can do about Actions is decided by what Forgejo will say.
//
// The workflows themselves need nothing here: they are files under
// .forgejo/workflows in the repository, so git replication has already put
// them on every node.
//
// Variables can be read back, so they travel: "<name>:<digest of its
// value>" members merged with internal/set, and changing a value is one
// member going and another arriving, written as an update so a workflow
// never catches the variable missing.
//
// Secrets cannot. Forgejo gives back a secret's name and when it was set,
// never what is in it, so nothing can copy one. What ForgeSync can do is
// notice that a node hasn't got a secret the others have -- which is
// otherwise invisible until a workflow fails there, and is exactly the
// sort of difference that belongs with a person rather than in a log.
//
// Runs and runners stay where they are. A run is the record of a machine
// having executed something; copying one to a node that didn't run it
// would be a lie, and a runner is registered to the node it belongs to.

// The conflict kinds this pass owns.
const (
	// SecretConflictKind: a node hasn't got a secret the others have.
	SecretConflictKind = "actions_secret_missing"
	// VariableConflictKind: the same variable was given two different
	// values on two nodes. ForgeSync doesn't choose between them.
	VariableConflictKind = "actions_variable_conflict"
)

// variableMember identifies a variable and what is in it.
func variableMember(v forgejo.ActionVariable) string {
	sum := sha256.Sum256([]byte(v.Data))
	return v.Name + ":" + hex.EncodeToString(sum[:])[:12]
}

// variableNameOf is the name in a member.
func variableNameOf(member string) string {
	i := strings.LastIndex(member, ":")
	if i < 0 {
		return member
	}
	return member[:i]
}

// syncActions brings one repository's variables together and says which
// nodes are missing a secret.
func (e *Engine) syncActions(ctx context.Context, rec store.RepositoryRecord, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	at := map[string]map[string]forgejo.ActionVariable{}
	have := map[string]map[string]bool{}
	secrets := map[string]map[string]bool{}
	unread := 0 // nodes that have the repository and could not be read (see settle.go)
	for _, n := range e.order {
		if e.nodes[n].API == nil || !e.hasRepo(rec, n) {
			continue
		}
		if !healthy[n] {
			unread++
			continue
		}
		vars, err := e.nodes[n].API.ActionVariables(ctx, owner, name)
		if forgejo.IsNotFound(err) {
			// Actions are turned off for this repository on this node,
			// which is a node's own decision (has_actions isn't
			// replicated, see metadata.go). Nothing to compare, and
			// nothing worth saying every round.
			continue
		}
		if err != nil {
			e.log.Warn("actions: reading the variables failed", "repository", rec.FullName, "node", n, "error", err)
			unread++
			continue
		}
		secs, err := e.nodes[n].API.ActionSecrets(ctx, owner, name)
		if forgejo.IsNotFound(err) {
			continue
		}
		if err != nil {
			e.log.Warn("actions: reading the secrets failed", "repository", rec.FullName, "node", n, "error", err)
			unread++
			continue
		}
		at[n], have[n], secrets[n] = map[string]forgejo.ActionVariable{}, map[string]bool{}, map[string]bool{}
		for _, v := range vars {
			at[n][variableMember(v)], have[n][variableMember(v)] = v, true
		}
		for _, s := range secs {
			secrets[n][s.Name] = true
		}
	}
	if len(have) < 2 {
		return
	}
	found := e.mergeVariables(ctx, rec, owner, name, at, have, unread)
	found = append(found, secretConflicts(e.order, rec, secrets)...)
	kinds := []string{VariableConflictKind, SecretConflictKind}
	if _, err := e.store.SyncConflicts(ctx, found, []string{rec.ID}, kinds, e.now()); err != nil {
		e.log.Error("actions: recording the conflicts failed", "repository", rec.FullName, "error", err)
	}
}

// mergeVariables brings the variables together and reports the names two
// nodes disagree about.
func (e *Engine) mergeVariables(ctx context.Context, rec store.RepositoryRecord, owner, name string,
	at map[string]map[string]forgejo.ActionVariable, have map[string]map[string]bool, unread int) []store.FoundConflict {
	base := set.From(rec.BaseActionVariables)
	plan := set.Decide(have, base)
	contested := contestedVariables(plan)
	for _, n := range e.order {
		if _, taking := have[n]; !taking {
			continue
		}
		changing := map[string]bool{}
		for _, m := range plan.Add[n] {
			if !contested[variableNameOf(m)] {
				changing[variableNameOf(m)] = true
			}
		}
		for _, m := range plan.Remove[n] {
			if contested[variableNameOf(m)] {
				continue // nobody's value is written over
			}
			// A variable whose value is only changing is updated below, so
			// a workflow never catches it missing.
			if changing[variableNameOf(m)] {
				delete(have[n], m)
				continue
			}
			if err := e.nodes[n].API.DeleteActionVariable(ctx, owner, name, variableNameOf(m)); err != nil {
				e.log.Warn("actions: removing a variable failed; left for the next run", "repository", rec.FullName,
					"node", n, "variable", variableNameOf(m), "error", err)
				continue
			}
			delete(have[n], m)
			e.log.Info("variable removed", "repository", rec.FullName, "node", n, "variable", variableNameOf(m))
		}
		for _, m := range plan.Add[n] {
			if contested[variableNameOf(m)] {
				continue
			}
			want, _, found := variableFrom(e.order, at, m)
			if !found {
				continue
			}
			var err error
			if changing[variableNameOf(m)] && hasVariableNamed(at[n], variableNameOf(m)) {
				err = e.nodes[n].API.UpdateActionVariable(ctx, owner, name, want.Name, want.Data)
			} else {
				err = e.nodes[n].API.CreateActionVariable(ctx, owner, name, want.Name, want.Data)
			}
			if err != nil {
				e.log.Warn("actions: writing a variable failed; left for the next run", "repository", rec.FullName,
					"node", n, "variable", want.Name, "error", err)
				continue
			}
			have[n][m] = true
			e.log.Info("variable written", "repository", rec.FullName, "node", n, "variable", want.Name)
		}
	}
	if now := settle(unread, rec.BaseActionVariables, have, base); now != rec.BaseActionVariables {
		if err := e.store.SetRepositoryActionVariables(ctx, rec.ID, now); err != nil {
			e.log.Error("actions: recording the variables failed", "repository", rec.FullName, "error", err)
		}
	}
	return variableConflicts(e.order, rec, at, contested)
}

// contestedVariables are the names given two different new values at once.
// One new value anywhere is the change everyone takes; two are a
// disagreement only the owner can settle, so neither is written.
func contestedVariables(plan set.Plan) map[string]bool {
	values := map[string]map[string]bool{}
	for _, members := range plan.Add {
		for _, m := range members {
			n := variableNameOf(m)
			if values[n] == nil {
				values[n] = map[string]bool{}
			}
			values[n][m] = true
		}
	}
	out := map[string]bool{}
	for n, vs := range values {
		if len(vs) > 1 {
			out[n] = true
		}
	}
	return out
}

// variableConflicts describes each contested name and what every node has.
func variableConflicts(order []string, rec store.RepositoryRecord, at map[string]map[string]forgejo.ActionVariable,
	contested map[string]bool) []store.FoundConflict {
	names := make([]string, 0, len(contested))
	for n := range contested {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []store.FoundConflict
	for _, name := range names {
		nodes := map[string]any{}
		for _, n := range order {
			for _, v := range at[n] {
				if v.Name == name {
					nodes[n] = v.Data
				}
			}
		}
		out = append(out, store.FoundConflict{RepositoryID: rec.ID, Kind: VariableConflictKind, Ref: name,
			Details: map[string]any{"variable": name, "values": nodes}})
	}
	return out
}

// secretConflicts says which nodes are missing a secret the others have.
// ForgeSync can't copy one -- Forgejo never gives a value back -- so this
// is a conflict for a person, and otherwise it stays invisible until a
// workflow fails on that node.
func secretConflicts(order []string, rec store.RepositoryRecord, secrets map[string]map[string]bool) []store.FoundConflict {
	everywhere := map[string]bool{}
	for _, on := range secrets {
		for s := range on {
			everywhere[s] = true
		}
	}
	names := make([]string, 0, len(everywhere))
	for s := range everywhere {
		names = append(names, s)
	}
	sort.Strings(names)

	var found []store.FoundConflict
	for _, s := range names {
		var missing []string
		for _, n := range order {
			if on, asked := secrets[n]; asked && !on[s] {
				missing = append(missing, n)
			}
		}
		if len(missing) == 0 {
			continue
		}
		found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: SecretConflictKind, Ref: s,
			Details: map[string]any{"secret": s, "missing": missing}})
	}
	return found
}

func variableFrom(order []string, at map[string]map[string]forgejo.ActionVariable, member string) (forgejo.ActionVariable, string, bool) {
	for _, n := range order {
		if v, ok := at[n][member]; ok {
			return v, n, true
		}
	}
	return forgejo.ActionVariable{}, "", false
}

func hasVariableNamed(vars map[string]forgejo.ActionVariable, name string) bool {
	for _, v := range vars {
		if v.Name == name {
			return true
		}
	}
	return false
}
