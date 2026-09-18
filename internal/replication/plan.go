// Package replication copies Git branches and tags from a repository's
// primary node to its replicas.
//
// It never overwrites work. Every change it makes is a fast-forward, a new
// ref, or a deletion of something it wrote itself, and every push is a
// compare-and-swap (--force-with-lease) against the value it expects on the
// replica. Everything else becomes a conflict for a person to resolve.
//
// To tell "deleted on the primary" from "created on the replica", it keeps
// the last value it wrote to each replica (the base of a three-way compare).
package replication

import "sort"

// Refs maps full ref names (refs/heads/main, refs/tags/v1) to commit IDs.
type Refs map[string]string

// ActionKind is what to do with one ref on a replica.
type ActionKind string

const (
	Create      ActionKind = "create"       // ref missing on the replica
	FastForward ActionKind = "fast_forward" // replica is behind the primary
	Delete      ActionKind = "delete"       // deleted on the primary; replica unchanged since we wrote it
)

// Action is one ref update to push. Expected is the value the replica must
// still have for the push to go ahead ("" = must not exist).
type Action struct {
	Kind     ActionKind `json:"kind"`
	Ref      string     `json:"ref"`
	Expected string     `json:"expected,omitempty"`
	Target   string     `json:"target,omitempty"` // "" for Delete
}

// IssueKind is a difference replication won't resolve by itself. Each maps
// to a conflict kind.
type IssueKind string

const (
	// The replica has commits on this branch that the primary doesn't.
	ReplicaAhead IssueKind = "git_replica_ahead"
	// Primary and replica each have commits the other lacks.
	Diverged IssueKind = "git_diverged"
	// The primary rewrote history (force-push or moved tag) since the last
	// replication; copying it would discard what the replica has.
	PrimaryRewrote IssueKind = "git_primary_rewrote"
	// A tag differs on the replica, or a ref there was changed since the
	// last replication.
	ReplicaChanged IssueKind = "git_replica_changed"
	// A ref exists only on the replica and wasn't put there by ForgeSync.
	ReplicaExtra IssueKind = "git_replica_extra_ref"
)

// ConflictKinds are the conflict kinds replication owns: it opens them and
// clears them once they're gone.
var ConflictKinds = []string{string(ReplicaAhead), string(Diverged), string(PrimaryRewrote), string(ReplicaChanged), string(ReplicaExtra)}

// Issue is a ref that needs a person.
type Issue struct {
	Kind    IssueKind `json:"kind"`
	Ref     string    `json:"ref"`
	Primary string    `json:"primary"` // commit on the primary, "" if absent
	Replica string    `json:"replica"` // commit on the replica, "" if absent
}

// Ancestry answers "is a an ancestor of b?" (a == b counts). ok is false if
// it couldn't tell, e.g. objects are missing.
type Ancestry func(a, b string) (is, ok bool)

// Plan decides what to do on one replica, given the refs on the primary, on
// the replica, and the ones ForgeSync last wrote there (base).
func Plan(primary, replica, base Refs, isAncestor Ancestry) ([]Action, []Issue) {
	var actions []Action
	var issues []Issue
	for _, ref := range union(primary, replica, base) {
		p, hasP := primary[ref]
		r, hasR := replica[ref]
		b, hasB := base[ref]
		tag := isTag(ref)
		switch {
		case hasP && hasR && p == r:
			// In sync.
		case hasP && !hasR:
			// Missing on the replica: new on the primary, or deleted on the
			// replica (which is read-only by design). Either way creating it
			// loses nothing.
			actions = append(actions, Action{Kind: Create, Ref: ref, Target: p})
		case hasP && hasR:
			actions, issues = planChange(actions, issues, ref, p, r, b, hasB, tag, isAncestor)
		case !hasP && hasR && hasB && b == r:
			// Deleted on the primary, and the replica still has exactly what
			// ForgeSync wrote.
			actions = append(actions, Action{Kind: Delete, Ref: ref, Expected: r})
		case !hasP && hasR && hasB:
			// Deleted on the primary but changed on the replica since.
			issues = append(issues, Issue{Kind: ReplicaChanged, Ref: ref, Replica: r})
		case !hasP && hasR:
			issues = append(issues, Issue{Kind: ReplicaExtra, Ref: ref, Replica: r})
		}
	}
	return actions, issues
}

func planChange(actions []Action, issues []Issue, ref, p, r, b string, hasB, tag bool, isAncestor Ancestry) ([]Action, []Issue) {
	issue := func(k IssueKind) []Issue { return append(issues, Issue{Kind: k, Ref: ref, Primary: p, Replica: r}) }
	ff := Action{Kind: FastForward, Ref: ref, Expected: r, Target: p}

	if tag {
		// Tags aren't expected to move. If the replica still has the value
		// ForgeSync wrote, the tag was moved on the primary; otherwise it was
		// changed on the replica. Both need a person.
		if hasB && b == r {
			return actions, issue(PrimaryRewrote)
		}
		return actions, issue(ReplicaChanged)
	}

	behind, ok := isAncestor(r, p)
	if !ok {
		return actions, issue(Diverged) // can't prove it's safe
	}
	if behind {
		return append(actions, ff), issues
	}
	if hasB && b == r {
		// The replica is as ForgeSync left it, but the primary no longer
		// contains that commit: history was rewritten on the primary.
		return actions, issue(PrimaryRewrote)
	}
	ahead, ok := isAncestor(p, r)
	if ok && ahead {
		return actions, issue(ReplicaAhead)
	}
	return actions, issue(Diverged)
}

func isTag(ref string) bool { return len(ref) > 10 && ref[:10] == "refs/tags/" }

// Replicated reports whether a ref is one replication manages.
func Replicated(ref string) bool {
	return isTag(ref) || (len(ref) > 11 && ref[:11] == "refs/heads/")
}

func union(maps ...Refs) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
