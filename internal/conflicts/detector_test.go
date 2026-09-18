package conflicts

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// graph is one node's copy of a repository: commit -> parent ("" for root).
type graph map[string]string

func (g graph) ancestors(c string) map[string]bool {
	out := map[string]bool{}
	for c != "" {
		if _, ok := g[c]; !ok {
			break
		}
		out[c] = true
		c = g[c]
	}
	return out
}

// fakeNode answers CommitsAhead from its graph, like Forgejo's compare API.
type fakeNode struct {
	g   graph
	err error
}

func (f fakeNode) CommitsAhead(_ context.Context, _, _, base, head string) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	if _, ok := f.g[base]; !ok {
		return 0, false, nil
	}
	if _, ok := f.g[head]; !ok {
		return 0, false, nil
	}
	b := f.g.ancestors(base)
	n := 0
	for c := range f.g.ancestors(head) {
		if !b[c] {
			n++
		}
	}
	return n, true, nil
}

var t0 = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

func rec(replicas ...store.Replica) store.RepositoryRecord {
	return store.RepositoryRecord{ID: "repo-1", FullName: "alice/demo", PrimaryNode: "se", FirstSeenAt: t0, Replicas: replicas}
}

func head(node, sha string) store.Replica {
	return store.Replica{Node: node, Present: true, DefaultBranch: "main", HeadSHA: sha}
}

func scans(nodes ...string) map[string]store.NodeScan {
	later := t0.Add(time.Hour)
	m := map[string]store.NodeScan{}
	for _, n := range nodes {
		m[n] = store.NodeScan{Node: n, OK: true, LastSuccessAt: &later}
	}
	return m
}

func detector(nodes map[string]Comparer, names ...string) *Detector {
	return NewDetector(names, nodes, nil, slog.New(slog.DiscardHandler))
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	linear := graph{"A": "", "B": "A"}
	branched := graph{"A": "", "C": "A"}

	t.Run("one node behind is not a conflict", func(t *testing.T) {
		d := detector(map[string]Comparer{"se": fakeNode{g: linear}, "dk": fakeNode{g: graph{"A": ""}}}, "se", "dk")
		found, ok := d.check(ctx, rec(head("se", "B"), head("dk", "A")), scans("se", "dk"))
		if len(found) != 0 || !ok {
			t.Fatalf("found %+v, conclusive %t", found, ok)
		}
	})

	t.Run("diverged history", func(t *testing.T) {
		d := detector(map[string]Comparer{"se": fakeNode{g: linear}, "dk": fakeNode{g: branched}}, "se", "dk")
		found, ok := d.check(ctx, rec(head("se", "B"), head("dk", "C")), scans("se", "dk"))
		if len(found) != 1 || !ok {
			t.Fatalf("found %+v, conclusive %t", found, ok)
		}
		f := found[0]
		rels := f.Details["relations"].([]map[string]string)
		if f.Kind != KindDiverged || f.Ref != "refs/heads/main" || len(rels) != 1 || rels[0]["relation"] != RelDiverged ||
			f.Details["heads"].(map[string]string)["dk"] != "C" || f.Details["primary"] != "se" {
			t.Errorf("conflict = %+v", f)
		}
	})

	t.Run("three nodes: one behind, two diverged", func(t *testing.T) {
		d := detector(map[string]Comparer{
			"se": fakeNode{g: linear}, "dk": fakeNode{g: graph{"A": ""}}, "de": fakeNode{g: branched},
		}, "se", "dk", "de")
		found, ok := d.check(ctx, rec(head("se", "B"), head("dk", "A"), head("de", "C")), scans("se", "dk", "de"))
		if len(found) != 1 || !ok {
			t.Fatalf("found %+v, conclusive %t", found, ok)
		}
		got := map[string]string{}
		for _, r := range found[0].Details["relations"].([]map[string]string) {
			got[r["a"]+"/"+r["b"]] = r["relation"]
		}
		want := map[string]string{"se/dk": RelBBehind, "se/de": RelDiverged, "dk/de": RelABehind}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
			}
		}
	})

	t.Run("different default branches", func(t *testing.T) {
		d := detector(map[string]Comparer{}, "se", "dk")
		trunk := head("dk", "A")
		trunk.DefaultBranch = "trunk"
		found, ok := d.check(ctx, rec(head("se", "A"), trunk), scans("se", "dk"))
		if len(found) != 1 || found[0].Kind != KindBranchMismatch || !ok {
			t.Fatalf("found %+v, conclusive %t", found, ok)
		}
	})

	t.Run("comparison fails: nothing found, not conclusive", func(t *testing.T) {
		d := detector(map[string]Comparer{"se": fakeNode{err: errors.New("timeout")}, "dk": fakeNode{g: branched}}, "se", "dk")
		found, ok := d.check(ctx, rec(head("se", "B"), head("dk", "C")), scans("se", "dk"))
		if len(found) != 0 || ok {
			t.Fatalf("found %+v, conclusive %t", found, ok)
		}
	})

	t.Run("stale node data is not conclusive", func(t *testing.T) {
		d := detector(map[string]Comparer{"se": fakeNode{g: linear}, "dk": fakeNode{g: linear}}, "se", "dk")
		sc := scans("se", "dk")
		stale := sc["dk"]
		stale.OK = false
		sc["dk"] = stale
		_, ok := d.check(ctx, rec(head("se", "B"), head("dk", "B")), sc)
		if ok {
			t.Fatal("conclusive despite a failed latest scan")
		}
	})

	t.Run("an empty copy is ignored", func(t *testing.T) {
		d := detector(map[string]Comparer{}, "se", "dk")
		found, ok := d.check(ctx, rec(head("se", "B"), store.Replica{Node: "dk", Present: true, Empty: true}), scans("se", "dk"))
		if len(found) != 0 || !ok {
			t.Fatalf("found %+v, conclusive %t", found, ok)
		}
	})
}

type fakeStore struct {
	recs    []store.RepositoryRecord
	found   []store.FoundConflict
	checked []string
	audit   []string
	calls   []syncCall
}

func (f *fakeStore) Repositories(context.Context) ([]store.RepositoryRecord, error) {
	return f.recs, nil
}
func (f *fakeStore) NodeScans(context.Context) ([]store.NodeScan, error) {
	var out []store.NodeScan
	for _, s := range scans("se", "dk") {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeStore) SyncConflicts(_ context.Context, found []store.FoundConflict, checked, kinds []string, _ time.Time) ([]store.ConflictChange, error) {
	f.calls = append(f.calls, syncCall{found, checked, kinds})
	if len(f.calls) > 1 {
		return nil, nil
	}
	f.found, f.checked = found, checked
	return []store.ConflictChange{{ID: 7, FullName: "alice/demo", Kind: KindDiverged, Change: "opened"}}, nil
}

type syncCall struct {
	found          []store.FoundConflict
	checked, kinds []string
}

func (f *fakeStore) Audit(_ context.Context, actor, action, target string, _ map[string]any) error {
	f.audit = append(f.audit, actor+" "+action+" "+target)
	return nil
}

func TestRun(t *testing.T) {
	st := &fakeStore{recs: []store.RepositoryRecord{
		rec(head("se", "B"), head("dk", "C")),
		{ID: "repo-2", FullName: "bob/tools", FirstSeenAt: t0, Replicas: []store.Replica{head("se", "A"), head("dk", "A")}},
	}}
	d := NewDetector([]string{"se", "dk"}, map[string]Comparer{
		"se": fakeNode{g: graph{"A": "", "B": "A"}}, "dk": fakeNode{g: graph{"A": "", "C": "A"}},
	}, st, slog.New(slog.DiscardHandler))
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.found) != 1 || len(st.checked) != 2 {
		t.Errorf("found %d, checked %v", len(st.found), st.checked)
	}
	if len(st.audit) != 1 || st.audit[0] != "forgesync conflict.opened alice/demo" {
		t.Errorf("audit = %v", st.audit)
	}
}

func TestRunLeavesPrimariedRepositoriesToReplication(t *testing.T) {
	trunk := head("dk", "C")
	trunk.DefaultBranch = "trunk"
	st := &fakeStore{recs: []store.RepositoryRecord{
		rec(head("se", "B"), head("dk", "C")), // has primary "se", diverged
		{ID: "repo-3", FullName: "carol/site", PrimaryNode: "se", FirstSeenAt: t0, Replicas: []store.Replica{head("se", "A"), trunk}},
		{ID: "repo-2", FullName: "bob/tools", FirstSeenAt: t0, Replicas: []store.Replica{head("se", "B"), head("dk", "C")}},
	}}
	d := NewDetector([]string{"se", "dk"}, map[string]Comparer{
		"se": fakeNode{g: graph{"A": "", "B": "A"}}, "dk": fakeNode{g: graph{"A": "", "C": "A"}},
	}, st, slog.New(slog.DiscardHandler))
	d.ReplicationOwnsPrimaries = true
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.calls) != 2 {
		t.Fatalf("calls = %+v", st.calls)
	}
	own, prim := st.calls[0], st.calls[1]
	// Without a primary: the detector owns everything.
	if len(own.found) != 1 || own.found[0].RepositoryID != "repo-2" || len(own.checked) != 1 || own.checked[0] != "repo-2" {
		t.Errorf("own scope = %+v", own)
	}
	// With a primary: only branch mismatches, and only that kind is cleared.
	if len(prim.found) != 1 || prim.found[0].Kind != KindBranchMismatch || prim.found[0].RepositoryID != "repo-3" ||
		len(prim.kinds) != 1 || prim.kinds[0] != KindBranchMismatch || len(prim.checked) != 2 {
		t.Errorf("primaried scope = %+v", prim)
	}
}
