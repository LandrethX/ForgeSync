package inventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

var (
	t0    = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	nodes = []string{"dk", "se"}
)

func scanned(at time.Time, ok bool) store.NodeScan {
	return store.NodeScan{OK: ok, LastSuccessAt: &at}
}

func TestCompare(t *testing.T) {
	head := func(node, branch, sha string) store.Replica {
		return store.Replica{Node: node, Present: true, DefaultBranch: branch, HeadSHA: sha}
	}
	bothScanned := map[string]store.NodeScan{"dk": scanned(t0.Add(time.Hour), true), "se": scanned(t0.Add(time.Hour), true)}
	cases := map[string]struct {
		replicas []store.Replica
		scans    map[string]store.NodeScan
		want     Status
		presence []Presence
	}{
		"same everywhere": {
			replicas: []store.Replica{head("dk", "main", "aaa"), head("se", "main", "aaa")},
			scans:    bothScanned, want: Same, presence: []Presence{Present, Present},
		},
		"different commit": {
			replicas: []store.Replica{head("dk", "main", "aaa"), head("se", "main", "bbb")},
			scans:    bothScanned, want: Differs,
		},
		"different default branch": {
			replicas: []store.Replica{head("dk", "main", "aaa"), head("se", "trunk", "aaa")},
			scans:    bothScanned, want: Differs,
		},
		"empty everywhere": {
			replicas: []store.Replica{{Node: "dk", Present: true, Empty: true}, {Node: "se", Present: true, Empty: true}},
			scans:    bothScanned, want: Same,
		},
		"deleted on one node": {
			replicas: []store.Replica{head("dk", "main", "aaa"), {Node: "se", Present: false}},
			scans:    bothScanned, want: Missing, presence: []Presence{Present, Absent},
		},
		"never on a node that was scanned since it appeared": {
			replicas: []store.Replica{head("se", "main", "aaa")},
			scans:    bothScanned, want: Missing, presence: []Presence{Absent, Present},
		},
		"node not scanned since it appeared": {
			replicas: []store.Replica{head("se", "main", "aaa")},
			scans:    map[string]store.NodeScan{"dk": scanned(t0.Add(-time.Hour), true), "se": scanned(t0.Add(time.Hour), true)},
			want:     Unknown, presence: []Presence{NotKnownYet, Present},
		},
		"node never scanned": {
			replicas: []store.Replica{head("se", "main", "aaa")},
			scans:    map[string]store.NodeScan{"se": scanned(t0.Add(time.Hour), true)},
			want:     Unknown,
		},
		"branch head unreadable": {
			replicas: []store.Replica{head("dk", "main", "aaa"), {Node: "se", Present: true, DefaultBranch: "main", HeadError: "404"}},
			scans:    bothScanned, want: Unknown,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := store.RepositoryRecord{FirstSeenAt: t0, Replicas: tc.replicas}
			got, views := Compare(rec, nodes, tc.scans)
			if got != tc.want {
				t.Errorf("status = %s, want %s", got, tc.want)
			}
			for i, p := range tc.presence {
				if views[i].Presence != p {
					t.Errorf("%s presence = %s, want %s", views[i].Node, views[i].Presence, p)
				}
			}
		})
	}

	// A failed latest scan marks the node's data as stale but keeps it.
	scans := map[string]store.NodeScan{"dk": scanned(t0.Add(time.Hour), false), "se": scanned(t0.Add(time.Hour), true)}
	_, views := Compare(store.RepositoryRecord{FirstSeenAt: t0, Replicas: []store.Replica{head("dk", "main", "a"), head("se", "main", "a")}}, nodes, scans)
	if !views[0].Stale || views[1].Stale {
		t.Errorf("stale flags = %v, %v", views[0].Stale, views[1].Stale)
	}
}

// fakeForgejo serves n repositories, page by page.
type fakeForgejo struct {
	repos     []forgejo.Repository
	users     []forgejo.User // of every login source; ListUsers filters
	listErr   error
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func (f *fakeForgejo) ListRepos(_ context.Context, page, limit int) ([]forgejo.Repository, int, error) {
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	start := (page - 1) * limit
	if start >= len(f.repos) {
		return nil, len(f.repos), nil
	}
	end := min(start+limit, len(f.repos))
	out := append([]forgejo.Repository(nil), f.repos[start:end]...)
	if page == 2 {
		// Simulate a repository created during paging: page 2 repeats the
		// last repository of page 1.
		out = append([]forgejo.Repository{f.repos[start-1]}, out...)
	}
	return out, len(f.repos), nil
}

func (f *fakeForgejo) ListUsers(_ context.Context, src int64, page, limit int) ([]forgejo.User, int, error) {
	var of []forgejo.User
	for _, u := range f.users {
		if u.SourceID == src {
			of = append(of, u)
		}
	}
	start := (page - 1) * limit
	if start >= len(of) {
		return nil, len(of), nil
	}
	return of[start:min(start+limit, len(of))], len(of), nil
}

func (f *fakeForgejo) GetRepo(_ context.Context, owner, name string) (forgejo.Repository, bool, error) {
	if f.listErr != nil {
		return forgejo.Repository{}, false, f.listErr
	}
	for _, r := range f.repos {
		if r.FullName == owner+"/"+name {
			return r, true, nil
		}
	}
	return forgejo.Repository{}, false, nil
}

func (f *fakeForgejo) BranchHead(_ context.Context, owner, repo, branch string) (string, error) {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxFlight.Load()
		if n <= m || f.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	if repo == "broken" {
		return "", errors.New("404 branch not found")
	}
	return "sha-" + owner + "-" + repo, nil
}

type fakeRecorder struct {
	mu       sync.Mutex
	scans    map[string][]store.ScannedRepo
	users    map[string][]store.ScannedUser
	failures map[string]error
	observed map[string]string
}

func (r *fakeRecorder) RecordRepoObservation(_ context.Context, node string, _ time.Time, name string, found bool, repo store.ScannedRepo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observed == nil {
		r.observed = map[string]string{}
	}
	if found {
		r.observed[node] = name + " " + repo.HeadSHA
	} else {
		r.observed[node] = name + " gone"
	}
	return nil
}

func (r *fakeRecorder) RecordNodeUsers(_ context.Context, node string, _ time.Time, users []store.ScannedUser) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users[node] = users
	return nil
}

func (r *fakeRecorder) RecordNodeScan(_ context.Context, node string, _, _ time.Time, repos []store.ScannedRepo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scans[node] = repos
	return nil
}

func (r *fakeRecorder) RecordNodeScanFailure(_ context.Context, node string, _, _ time.Time, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[node] = err
	return nil
}

func TestScanner(t *testing.T) {
	var repos []forgejo.Repository
	for i := 1; i <= 120; i++ {
		repos = append(repos, forgejo.Repository{ID: int64(i), Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("alice/r%d", i),
			Owner: forgejo.User{Login: "alice"}, DefaultBranch: "main"})
	}
	repos[4].Empty = true
	repos[5].Name, repos[5].FullName = "broken", "alice/broken"

	var users []forgejo.User
	for i := 1; i <= 60; i++ {
		users = append(users, forgejo.User{ID: int64(i), Login: fmt.Sprintf("u%d", i), SourceID: 2, LoginName: fmt.Sprintf("sub-%d", i)})
	}
	users = append(users, forgejo.User{ID: 99, Login: "siteadmin"}) // local: not listed

	se := &fakeForgejo{repos: repos, users: users}
	dk := &fakeForgejo{listErr: errors.New("connection refused")}
	rec := &fakeRecorder{scans: map[string][]store.ScannedRepo{}, users: map[string][]store.ScannedUser{}, failures: map[string]error{}}
	s := NewScanner([]Target{{Name: "se", Client: se, SceneIDSourceID: 2}, {Name: "dk", Client: dk, SceneIDSourceID: 1}},
		Options{Interval: time.Hour, BranchConcurrency: 3}, rec, slog.New(slog.DiscardHandler))

	after := 0
	s.opts.AfterScan = func(context.Context) {
		after++
		if len(rec.scans["se"]) == 0 || rec.failures["dk"] == nil {
			t.Error("AfterScan ran before every node was recorded")
		}
	}
	s.ScanAll(context.Background())
	if after != 1 {
		t.Errorf("AfterScan ran %d times", after)
	}

	got := rec.scans["se"]
	if len(got) != 120 {
		t.Fatalf("scanned %d repositories, want 120 (no duplicates, all pages)", len(got))
	}
	if got[0].HeadSHA != "sha-alice-r1" || got[4].HeadSHA != "" || !got[4].Empty || got[5].HeadError == "" {
		t.Errorf("heads: %+v / %+v / %+v", got[0], got[4], got[5])
	}
	if m := se.maxFlight.Load(); m > 3 {
		t.Errorf("%d branch lookups in parallel, limit is 3", m)
	}
	if u := rec.users["se"]; len(u) != 60 || u[59].Sub != "sub-60" {
		t.Errorf("users on se = %d, want the 60 SceneID users", len(u))
	}
	if rec.failures["dk"] == nil || rec.scans["dk"] != nil || rec.users["dk"] != nil {
		t.Errorf("dk failure not recorded: %v", rec.failures)
	}
}

func TestTrigger(t *testing.T) {
	s := NewScanner(nil, Options{Interval: time.Hour}, &fakeRecorder{}, slog.New(slog.DiscardHandler))
	if !s.Trigger() {
		t.Fatal("first trigger refused")
	}
	if s.Trigger() {
		t.Fatal("second trigger accepted while one is queued")
	}
	s.running.Store(true)
	<-s.trigger
	if s.Trigger() {
		t.Fatal("trigger accepted while a scan is running")
	}
}

func TestScannerSkipsOwners(t *testing.T) {
	repos := []forgejo.Repository{
		{ID: 1, Name: "a", FullName: "alice/a", Owner: forgejo.User{Login: "alice"}, Empty: true},
		{ID: 2, Name: "alice--a--20260919-103000", FullName: "forgesync-archive/alice--a--20260919-103000",
			Owner: forgejo.User{Login: "forgesync-archive"}, Empty: true},
	}
	rec := &fakeRecorder{scans: map[string][]store.ScannedRepo{}, users: map[string][]store.ScannedUser{}, failures: map[string]error{}}
	s := NewScanner([]Target{{Name: "se", Client: &fakeForgejo{repos: repos}}},
		Options{Interval: time.Hour, SkipOwners: []string{"ForgeSync-Archive"}}, rec, slog.New(slog.DiscardHandler))
	s.ScanAll(context.Background())
	if got := rec.scans["se"]; len(got) != 1 || got[0].FullName != "alice/a" {
		t.Errorf("scanned %+v, want only alice/a", got)
	}
}

func TestScanRepo(t *testing.T) {
	se := &fakeForgejo{repos: []forgejo.Repository{{ID: 1, Name: "a", FullName: "alice/a", Owner: forgejo.User{Login: "alice"}, DefaultBranch: "main"}}}
	dk := &fakeForgejo{}
	de := &fakeForgejo{listErr: errors.New("connection refused")}
	rec := &fakeRecorder{}
	s := NewScanner([]Target{{Name: "se", Client: se}, {Name: "dk", Client: dk}, {Name: "de", Client: de}},
		Options{Interval: time.Hour}, rec, slog.New(slog.DiscardHandler))
	s.ScanRepo(context.Background(), "alice/a")
	if rec.observed["se"] != "alice/a sha-alice-a" || rec.observed["dk"] != "alice/a gone" || rec.observed["de"] != "" {
		t.Errorf("observed = %v (de failed, so nothing is recorded for it)", rec.observed)
	}
}
