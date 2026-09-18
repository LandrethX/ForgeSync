package replication

import (
	"reflect"
	"testing"
)

// history: commit -> parent. A-B-C is one line; A-X branches off A.
var history = map[string]string{"A": "", "B": "A", "C": "B", "X": "A"}

func ancestry(a, b string) (bool, bool) {
	if _, ok := history[a]; !ok {
		return false, false
	}
	for c := b; c != ""; c = history[c] {
		if c == a {
			return true, true
		}
	}
	return false, true
}

func TestPlan(t *testing.T) {
	const main, tag = "refs/heads/main", "refs/tags/v1"
	cases := map[string]struct {
		primary, replica, base Refs
		actions                []Action
		issues                 []Issue
	}{
		"in sync": {
			primary: Refs{main: "C"}, replica: Refs{main: "C"}, base: Refs{main: "C"},
		},
		"new branch on the primary": {
			primary: Refs{main: "C"}, replica: Refs{},
			actions: []Action{{Kind: Create, Ref: main, Target: "C"}},
		},
		"deleted on the replica: recreated": {
			primary: Refs{main: "C"}, replica: Refs{}, base: Refs{main: "C"},
			actions: []Action{{Kind: Create, Ref: main, Target: "C"}},
		},
		"replica behind": {
			primary: Refs{main: "C"}, replica: Refs{main: "A"}, base: Refs{main: "A"},
			actions: []Action{{Kind: FastForward, Ref: main, Expected: "A", Target: "C"}},
		},
		"replica behind, never replicated before": {
			primary: Refs{main: "C"}, replica: Refs{main: "B"},
			actions: []Action{{Kind: FastForward, Ref: main, Expected: "B", Target: "C"}},
		},
		"replica ahead: someone pushed to the replica": {
			primary: Refs{main: "B"}, replica: Refs{main: "C"}, base: Refs{main: "B"},
			issues: []Issue{{Kind: ReplicaAhead, Ref: main, Primary: "B", Replica: "C"}},
		},
		"diverged": {
			primary: Refs{main: "C"}, replica: Refs{main: "X"}, base: Refs{main: "A"},
			issues: []Issue{{Kind: Diverged, Ref: main, Primary: "C", Replica: "X"}},
		},
		"primary rewrote history since the last replication": {
			primary: Refs{main: "X"}, replica: Refs{main: "C"}, base: Refs{main: "C"},
			issues: []Issue{{Kind: PrimaryRewrote, Ref: main, Primary: "X", Replica: "C"}},
		},
		"ancestry unknown: treated as diverged, never pushed": {
			primary: Refs{main: "C"}, replica: Refs{main: "Z"},
			issues: []Issue{{Kind: Diverged, Ref: main, Primary: "C", Replica: "Z"}},
		},
		"deleted on the primary, replica unchanged": {
			primary: Refs{}, replica: Refs{main: "C"}, base: Refs{main: "C"},
			actions: []Action{{Kind: Delete, Ref: main, Expected: "C"}},
		},
		"deleted on the primary, but changed on the replica": {
			primary: Refs{}, replica: Refs{main: "C"}, base: Refs{main: "B"},
			issues: []Issue{{Kind: ReplicaChanged, Ref: main, Replica: "C"}},
		},
		"created on the replica": {
			primary: Refs{}, replica: Refs{"refs/heads/hotfix": "X"},
			issues: []Issue{{Kind: ReplicaExtra, Ref: "refs/heads/hotfix", Replica: "X"}},
		},
		"gone everywhere: nothing to do": {
			primary: Refs{}, replica: Refs{}, base: Refs{main: "C"},
		},
		"new tag": {
			primary: Refs{tag: "B"}, replica: Refs{},
			actions: []Action{{Kind: Create, Ref: tag, Target: "B"}},
		},
		"tag moved on the primary (not a fast-forward case, even if it is one)": {
			primary: Refs{tag: "C"}, replica: Refs{tag: "B"}, base: Refs{tag: "B"},
			issues: []Issue{{Kind: PrimaryRewrote, Ref: tag, Primary: "C", Replica: "B"}},
		},
		"tag differs on the replica": {
			primary: Refs{tag: "C"}, replica: Refs{tag: "X"},
			issues: []Issue{{Kind: ReplicaChanged, Ref: tag, Primary: "C", Replica: "X"}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			actions, issues := Plan(tc.primary, tc.replica, tc.base, ancestry)
			if !reflect.DeepEqual(actions, tc.actions) {
				t.Errorf("actions = %+v\nwant      %+v", actions, tc.actions)
			}
			if !reflect.DeepEqual(issues, tc.issues) {
				t.Errorf("issues = %+v\nwant     %+v", issues, tc.issues)
			}
		})
	}
}

func TestPlanManyRefsIsSortedAndIndependent(t *testing.T) {
	actions, issues := Plan(
		Refs{"refs/heads/b": "C", "refs/heads/a": "C", "refs/tags/t": "A"},
		Refs{"refs/heads/a": "X", "refs/heads/b": "B"},
		nil, ancestry)
	// One diverged branch doesn't hold back the others.
	if len(actions) != 2 || actions[0].Ref != "refs/heads/b" || actions[1].Ref != "refs/tags/t" {
		t.Errorf("actions = %+v", actions)
	}
	if len(issues) != 1 || issues[0].Ref != "refs/heads/a" {
		t.Errorf("issues = %+v", issues)
	}
}

func TestReplicated(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/heads/main": true, "refs/heads/feature/x": true, "refs/tags/v1.0": true,
		"refs/pull/1/head": false, "refs/notes/commits": false, "HEAD": false, "refs/heads/": false,
	} {
		if Replicated(ref) != want {
			t.Errorf("Replicated(%q) = %v", ref, !want)
		}
	}
}
