package replication

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

type memStore struct {
	mu       sync.Mutex
	rec      store.RepositoryRecord
	syncs    map[string]store.ReplicaSync
	refs     map[string]map[string]string
	found    []store.FoundConflict
	checked  []string
	audit    []string
	handoffs []store.Handoff
}

func newMemStore(rec store.RepositoryRecord) *memStore {
	return &memStore{rec: rec, syncs: map[string]store.ReplicaSync{}, refs: map[string]map[string]string{}}
}

func (m *memStore) Repositories(context.Context) ([]store.RepositoryRecord, error) {
	return []store.RepositoryRecord{m.rec}, nil
}
func (m *memStore) Repository(_ context.Context, id string) (store.RepositoryRecord, error) {
	if id != m.rec.ID {
		return store.RepositoryRecord{}, store.ErrNotFound
	}
	return m.rec, nil
}
func (m *memStore) ReplicatedRefs(_ context.Context, _, node string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.refs[node] {
		out[k] = v
	}
	return out, nil
}
func (m *memStore) ForgetReplicatedRefs(_ context.Context, _, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refs, node)
	return nil
}
func (m *memStore) Handoffs(_ context.Context, _ string, active bool) ([]store.Handoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Handoff
	for _, h := range m.handoffs {
		if !active || h.State == "open" || h.State == "kept_primary" {
			h.Nodes = append([]string(nil), h.Nodes...)
			out = append(out, h)
		}
	}
	return out, nil
}
func (m *memStore) SaveHandoff(_ context.Context, h store.Handoff) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h.ID = int64(len(m.handoffs) + 1)
	m.handoffs = append(m.handoffs, h)
	return h.ID, nil
}
func (m *memStore) UpdateHandoff(_ context.Context, h store.Handoff) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handoffs[h.ID-1] = h
	return nil
}
func (m *memStore) NoteCreatedAccount(_ context.Context, node, login string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, "noted "+node+"/"+login)
	return nil
}
func (m *memStore) SaveReplicaSync(_ context.Context, st store.ReplicaSync, refs map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncs[st.Node] = st
	if refs != nil {
		m.refs[st.Node] = refs
	}
	return nil
}
func (m *memStore) SyncConflicts(_ context.Context, found []store.FoundConflict, checked, _ []string, _ time.Time) ([]store.ConflictChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.found, m.checked = found, checked
	return nil, nil
}
func (m *memStore) Audit(_ context.Context, actor, action, target string, _ map[string]any) error {
	m.audit = append(m.audit, actor+" "+action)
	return nil
}

type fixedHealth map[string]health.State

func (f fixedHealth) Snapshot() []health.Status {
	var out []health.Status
	for n, s := range f {
		out = append(out, health.Status{Node: n, State: s})
	}
	return out
}

// setup: a primary "se" and replica "dk", each with alice/demo.
func setup(t *testing.T) (*Engine, *memStore, *gitNode, *gitNode, *workTree, fixedHealth) {
	t.Helper()
	se, dk := newGitNode(t), newGitNode(t)
	se.create("alice/demo")
	dk.create("alice/demo")
	st := newMemStore(store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"})
	h := fixedHealth{"se": health.Healthy, "dk": health.Healthy}
	e := NewEngine([]Node{
		{Name: "se", URL: se.srv.URL, User: testUser, Token: testToken},
		{Name: "dk", URL: dk.srv.URL, User: testUser, Token: testToken},
	}, testGit(t), st, h, Options{}, slog.New(slog.DiscardHandler))
	return e, st, se, dk, newWorkTree(t), h
}

func (m *memStore) sync(node string) store.ReplicaSync {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.syncs[node]
}

func TestEngineReplicates(t *testing.T) {
	e, st, se, dk, w, _ := setup(t)
	ctx := context.Background()

	// 1. First run: everything on the primary is created on the replica.
	w.commit("a")
	b := w.commit("b")
	runGit(t, w.dir, "tag", "v1")
	w.push(se, "alice/demo", "main", "main:refs/heads/dev", "refs/tags/v1")
	if err := e.RunRepo(ctx, st.rec); err != nil {
		t.Fatal(err)
	}
	if s := st.sync("dk"); s.State != StateSynced || s.RefsUpdated != 3 {
		t.Fatalf("first run: %+v", s)
	}
	if got := dk.refs("alice/demo"); got["refs/heads/main"] != b || got["refs/heads/dev"] != b || got["refs/tags/v1"] != b {
		t.Fatalf("dk = %v", got)
	}
	if len(st.checked) != 1 || len(st.found) != 0 {
		t.Errorf("conflicts: found %v checked %v", st.found, st.checked)
	}

	// 2. Primary moves on: fast-forward main, delete dev, add a branch.
	c := w.commit("c")
	w.push(se, "alice/demo", "main", "main:refs/heads/feature", ":refs/heads/dev")
	e.RunRepo(ctx, st.rec)
	got := dk.refs("alice/demo")
	if st.sync("dk").State != StateSynced || got["refs/heads/main"] != c || got["refs/heads/feature"] != c || got["refs/heads/dev"] != "" {
		t.Fatalf("second run: %+v / %v", st.sync("dk"), got)
	}

	// 3. Someone commits on the replica. That branch becomes a conflict and
	// isn't touched; other refs keep replicating.
	d := w.commit("d on dk")
	w.push(dk, "alice/demo", d+":refs/heads/main")
	runGit(t, w.dir, "tag", "v2", c)
	w.push(se, "alice/demo", "refs/tags/v2")
	e.RunRepo(ctx, st.rec)
	got = dk.refs("alice/demo")
	if st.sync("dk").State != StateConflict || got["refs/heads/main"] != d || got["refs/tags/v2"] != c {
		t.Fatalf("replica ahead: %+v / %v", st.sync("dk"), got)
	}
	if len(st.found) != 1 || st.found[0].Kind != string(ReplicaAhead) || st.found[0].Ref != "refs/heads/main" {
		t.Fatalf("found = %+v", st.found)
	}
	heads := st.found[0].Details["heads"].(map[string]string)
	if heads["se"] != c || heads["dk"] != d || st.found[0].Details["branch"] != "main" {
		t.Errorf("conflict details = %v", st.found[0].Details)
	}

	// 4. Someone fixes it: the primary takes dk's commit and moves on.
	w.push(se, "alice/demo", d+":refs/heads/main")
	e.RunRepo(ctx, st.rec)
	if st.sync("dk").State != StateSynced || len(st.found) != 0 || len(st.checked) != 1 {
		t.Fatalf("after the fix: %+v, found %v", st.sync("dk"), st.found)
	}

	// 5. The primary rewrites history (force-push). The replica keeps its
	// commits; a person has to confirm.
	runGit(t, w.dir, "checkout", "--quiet", "-b", "rewrite", c)
	x := w.commit("x replaces d")
	w.push(se, "alice/demo", x+":refs/heads/main")
	e.RunRepo(ctx, st.rec)
	if st.sync("dk").State != StateConflict || dk.refs("alice/demo")["refs/heads/main"] != d ||
		st.found[0].Kind != string(PrimaryRewrote) {
		t.Fatalf("rewrite: %+v / found %+v", st.sync("dk"), st.found)
	}
}

func TestEngineReplicaStates(t *testing.T) {
	ctx := context.Background()

	t.Run("replica missing the repository", func(t *testing.T) {
		e, st, se, dk, w, _ := setup(t)
		os.RemoveAll(filepath.Join(dk.root, "alice/demo.git"))
		w.commit("a")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateMissing || !strings.Contains(s.Detail, "doesn't exist on dk") {
			t.Errorf("state = %+v", s)
		}
		if len(st.checked) != 0 {
			t.Error("an incomplete run must not clear conflicts")
		}
	})

	t.Run("unhealthy replica waits", func(t *testing.T) {
		e, st, se, _, w, h := setup(t)
		h["dk"] = health.Unreachable
		w.commit("a")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateWaiting || len(st.checked) != 0 {
			t.Errorf("state = %+v, checked %v", s, st.checked)
		}
	})

	t.Run("unhealthy primary: nothing is read or written", func(t *testing.T) {
		e, st, se, dk, w, h := setup(t)
		h["se"] = health.AuthError
		w.commit("a")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateWaiting || len(dk.refs("alice/demo")) != 0 {
			t.Errorf("state = %+v", s)
		}
	})

	t.Run("replica refuses a ref", func(t *testing.T) {
		e, st, se, dk, w, _ := setup(t)
		hook := "#!/bin/sh\nwhile read old new ref; do [ \"$ref\" = refs/heads/locked ] && { echo 'protected' >&2; exit 1; }; done; exit 0\n"
		os.WriteFile(filepath.Join(dk.root, "alice/demo.git", "hooks", "pre-receive"), []byte(hook), 0o755)
		b := w.commit("b")
		w.push(se, "alice/demo", "main", "main:refs/heads/locked")
		e.RunRepo(ctx, st.rec)
		s := st.sync("dk")
		// git sends all refs in one push and the hook rejects the whole push,
		// so both fail; the detail names them.
		if s.State != StateError || !strings.Contains(s.Detail, "refs/heads/locked") {
			t.Errorf("state = %+v", s)
		}
		if st.refs["dk"]["refs/heads/locked"] == b {
			t.Error("a rejected ref was recorded as replicated")
		}
	})

	t.Run("primary missing the repository", func(t *testing.T) {
		e, st, se, _, _, _ := setup(t)
		os.RemoveAll(filepath.Join(se.root, "alice/demo.git"))
		if err := e.RunRepo(ctx, st.rec); !errors.Is(err, ErrRepoNotFound) {
			t.Errorf("err = %v", err)
		}
		if s := st.sync("dk"); s.State != StateError || !strings.Contains(s.Detail, "doesn't exist on the primary") {
			t.Errorf("state = %+v", s)
		}
	})
}

func TestEngineOneRunPerRepository(t *testing.T) {
	e, st, _, _, _, _ := setup(t)
	e.mu.Lock()
	e.running[st.rec.ID] = true
	e.mu.Unlock()
	if err := e.RunRepo(context.Background(), st.rec); !errors.Is(err, errBusy) {
		t.Errorf("err = %v", err)
	}
	if queued, err := e.Trigger(context.Background(), st.rec.ID); queued || err != nil {
		t.Errorf("trigger while running = %v, %v", queued, err)
	}
	st.rec.PrimaryNode = ""
	if _, err := e.Trigger(context.Background(), st.rec.ID); err == nil {
		t.Error("trigger without a primary accepted")
	}
}

func TestTriggerRunsAfterHook(t *testing.T) {
	e, st, se, _, w, _ := setup(t)
	w.commit("a")
	w.push(se, "alice/demo", "main")
	done := make(chan struct{})
	e.opts.AfterTriggered = func() { close(done) }
	if queued, err := e.Trigger(context.Background(), st.rec.ID); !queued || err != nil {
		t.Fatalf("trigger = %v, %v", queued, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AfterTriggered never ran")
	}
	if st.sync("dk").State != StateSynced {
		t.Errorf("state = %+v", st.sync("dk"))
	}
}
