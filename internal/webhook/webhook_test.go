package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
)

const master = "0123456789abcdef0123456789abcdef"

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

type recorder struct {
	mu      sync.Mutex
	changes []Change
	done    chan struct{}
}

func (r *recorder) Changed(_ context.Context, c Change) {
	r.mu.Lock()
	r.changes = append(r.changes, c)
	r.mu.Unlock()
	r.done <- struct{}{}
}

func TestReceiver(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 10)}
	tr := NewTracker([]string{"se", "dk"})
	rc := &Receiver{Secret: master, ServiceUsers: map[string]string{"se": "forgesync", "dk": "forgesync"},
		Dispatch: rec, Tracker: tr, Log: slog.New(slog.DiscardHandler), Context: context.Background()}

	post := func(node, event, body, sig string, gitea bool) int {
		req := httptest.NewRequest("POST", "/api/v1/hooks/forgejo/"+node, strings.NewReader(body))
		req.SetPathValue("node", node)
		if gitea {
			req.Header.Set("X-Gitea-Event", event)
			req.Header.Set("X-Gitea-Signature", sig)
		} else {
			req.Header.Set("X-Forgejo-Event", event)
			req.Header.Set("X-Forgejo-Signature", sig)
		}
		w := httptest.NewRecorder()
		rc.ServeHTTP(w, req)
		return w.Code
	}
	push := `{"ref":"refs/heads/main","repository":{"full_name":"alice/demo"},"pusher":{"login":"alice"},"sender":{"login":"alice"}}`
	se := NodeSecret(master, "se")

	if code := post("se", "push", push, sign(se, push), false); code != 202 {
		t.Fatalf("good delivery = %d", code)
	}
	<-rec.done
	if c := rec.changes[0]; c.Node != "se" || c.Event != "push" || c.Repository != "alice/demo" {
		t.Errorf("change = %+v", c)
	}
	// The older Gitea headers work too.
	if code := post("se", "delete", push, sign(se, push), true); code != 202 {
		t.Errorf("gitea headers = %d", code)
	}
	<-rec.done

	// Signed with another node's secret, unsigned, unknown node: refused.
	if code := post("dk", "push", push, sign(se, push), false); code != 401 {
		t.Errorf("se's signature on dk = %d", code)
	}
	if code := post("se", "push", push, "", false); code != 401 {
		t.Errorf("unsigned = %d", code)
	}
	if code := post("xx", "push", push, sign(NodeSecret(master, "xx"), push), false); code != 401 {
		t.Errorf("unknown node = %d", code)
	}
	if code := post("se", "push", push+" ", sign(se, push), false); code != 401 {
		t.Errorf("tampered body = %d", code)
	}

	// ForgeSync's own push, other events and test deliveries are accepted
	// but not acted on.
	own := strings.ReplaceAll(push, `"pusher":{"login":"alice"}`, `"pusher":{"login":"forgesync"}`)
	wiki := `{"action":"created","repository":{"full_name":"alice/demo"},"sender":{"login":"alice"}}`
	for _, d := range []struct{ event, body string }{{"push", own}, {"wiki", wiki}, {"push", `{"zen":"test"}`}} {
		if code := post("se", d.event, d.body, sign(se, d.body), false); code != 202 {
			t.Errorf("%s = %d", d.event, code)
		}
	}
	time.Sleep(50 * time.Millisecond)
	rec.mu.Lock()
	n := len(rec.changes)
	rec.mu.Unlock()
	if n != 2 {
		t.Errorf("%d changes dispatched, want 2: %+v", n, rec.changes)
	}
	st := tr.Snapshot()
	if st[0].Deliveries != 5 || st[0].Rejected != 2 || st[0].LastEvent != "push" || st[1].Rejected != 1 {
		t.Errorf("status = %+v", st)
	}
}

type fakeHooks struct {
	mu      sync.Mutex
	hooks   []forgejo.Hook
	next    int64
	secrets map[int64]string
	listErr error
	calls   []string
}

func (f *fakeHooks) SystemHooks(context.Context) ([]forgejo.Hook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forgejo.Hook(nil), f.hooks...), f.listErr
}
func (f *fakeHooks) CreateSystemHook(_ context.Context, url, secret string, events []string) (forgejo.Hook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	h := forgejo.Hook{ID: f.next, Config: map[string]string{"url": url}, Events: events, Active: true}
	f.hooks = append(f.hooks, h)
	if f.secrets == nil {
		f.secrets = map[int64]string{}
	}
	f.secrets[h.ID] = secret
	f.calls = append(f.calls, "create")
	return h, nil
}
func (f *fakeHooks) DeleteSystemHook(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, h := range f.hooks {
		if h.ID == id {
			f.hooks = append(f.hooks[:i], f.hooks[i+1:]...)
		}
	}
	f.calls = append(f.calls, "delete")
	return nil
}

func TestInstaller(t *testing.T) {
	base := "http://forgesync.test:8090/api/v1/hooks/forgejo"
	other := forgejo.Hook{ID: 100, Config: map[string]string{"url": "http://ci.example/hook"}, Events: []string{"push"}, Active: true}
	stale := forgejo.Hook{ID: 101, Config: map[string]string{"url": base + "/se?v=oldsecret"}, Events: Events, Active: true}
	api := &fakeHooks{hooks: []forgejo.Hook{other, stale}, next: 200}
	broken := &fakeHooks{listErr: errors.New("connection refused")}
	tr := NewTracker([]string{"se", "dk"})
	var audited []string
	in := &Installer{Targets: []Target{{"se", api}, {"dk", broken}}, BaseURL: base, Secret: master, Tracker: tr,
		Log: slog.New(slog.DiscardHandler),
		Audit: func(_ context.Context, action, target string, _ map[string]any) {
			audited = append(audited, action+" "+target)
		}}

	in.EnsureAll(context.Background())
	secret := NodeSecret(master, "se")
	if len(api.hooks) != 2 || api.hooks[0].ID != 100 || api.hooks[1].Config["url"] != HookURL(base, "se", secret) ||
		api.secrets[api.hooks[1].ID] != secret {
		t.Fatalf("hooks on se = %+v", api.hooks)
	}
	st := tr.Snapshot()
	if !st[0].Installed || st[0].HookID != 201 || st[1].Installed || !strings.Contains(st[1].Error, "refused") {
		t.Errorf("status = %+v", st)
	}
	if strings.Join(audited, ",") != "node.webhook_installed se" {
		t.Errorf("audit = %v", audited)
	}

	// A matching hook is kept as is, also when Forgejo lists more events
	// than asked for (it expands "issues").
	api.mu.Lock()
	api.hooks[1].Events = append(api.hooks[1].Events, "issue_assign", "issue_label", "issue_milestone")
	api.mu.Unlock()
	api.calls = nil
	in.EnsureAll(context.Background())
	if len(api.calls) != 0 {
		t.Errorf("second check changed things: %v", api.calls)
	}
	// A hook missing one of the events is replaced.
	api.mu.Lock()
	api.hooks[1].Events = []string{"push"}
	api.mu.Unlock()
	in.EnsureAll(context.Background())
	if strings.Join(api.calls, ",") != "delete,create" {
		t.Errorf("hook with too few events: %v", api.calls)
	}
	api.calls = nil
	// A different secret means a different URL: the hook is replaced.
	in.Secret = strings.Repeat("z", 32)
	in.EnsureAll(context.Background())
	if strings.Join(api.calls, ",") != "delete,create" || len(api.hooks) != 2 {
		t.Errorf("after a secret change: %v, %+v", api.calls, api.hooks)
	}
}

// After a failover the new leader's hook URL differs from the old one's
// only in its host. It's ForgeSync's own hook all the same, so the new
// leader takes it over instead of adding a second one beside it.
func TestTheOtherControllersHookIsTakenOver(t *testing.T) {
	const path = "/api/v1/hooks/forgejo"
	oldBase := "http://forgesync-a.test:8090" + path
	api := &fakeHooks{next: 300, hooks: []forgejo.Hook{
		{ID: 301, Config: map[string]string{"url": HookURL(oldBase, "se", NodeSecret(master, "se"))}, Events: Events, Active: true},
	}}
	newBase := "http://forgesync-b.test:8091" + path
	in := &Installer{Targets: []Target{{"se", api}}, BaseURL: newBase, Secret: master,
		Tracker: NewTracker([]string{"se"}), Log: slog.New(slog.DiscardHandler)}

	in.EnsureAll(context.Background())
	if strings.Join(api.calls, ",") != "delete,create" {
		t.Fatalf("calls = %v", api.calls)
	}
	if len(api.hooks) != 1 || api.hooks[0].Config["url"] != HookURL(newBase, "se", NodeSecret(master, "se")) {
		t.Errorf("hooks = %+v", api.hooks)
	}
}

type fakeLookup struct {
	ids  map[string]string
	prev map[string]string
}

func (f *fakeLookup) RepositoryIDByName(_ context.Context, name string) (string, error) {
	return f.ids[name], nil
}
func (f *fakeLookup) PreviousName(_ context.Context, _, name string) (string, error) {
	return f.prev[name], nil
}

type fakeScanner struct {
	scanned []string
	onScan  func(string)
}

func (f *fakeScanner) ScanRepo(_ context.Context, name string) {
	f.scanned = append(f.scanned, name)
	if f.onScan != nil {
		f.onScan(name)
	}
}

type fakeReplicator struct{ triggered []string }

func (f *fakeReplicator) Trigger(_ context.Context, id string) (bool, error) {
	f.triggered = append(f.triggered, id)
	return true, nil
}

func TestDispatcher(t *testing.T) {
	ctx := context.Background()
	newD := func() (*RepoDispatcher, *fakeLookup, *fakeScanner, *fakeReplicator, *int) {
		lk := &fakeLookup{ids: map[string]string{"alice/demo": "id-1"}, prev: map[string]string{}}
		sc, rp, assigned := &fakeScanner{}, &fakeReplicator{}, 0
		return &RepoDispatcher{Store: lk, Scanner: sc, Replicator: rp, Log: slog.New(slog.DiscardHandler),
			Assign: func(context.Context) error { assigned++; return nil }}, lk, sc, rp, &assigned
	}

	t.Run("a push to a known repository replicates it, nothing else", func(t *testing.T) {
		d, _, sc, rp, assigned := newD()
		d.Changed(ctx, Change{Node: "se", Event: "push", Repository: "alice/demo"})
		if strings.Join(rp.triggered, ",") != "id-1" || len(sc.scanned) != 0 || *assigned != 0 {
			t.Errorf("triggered %v, scanned %v, assigned %d", rp.triggered, sc.scanned, *assigned)
		}
	})

	t.Run("a new repository is looked at, given a primary, then replicated", func(t *testing.T) {
		d, lk, sc, rp, assigned := newD()
		sc.onScan = func(name string) { lk.ids[name] = "id-new" }
		d.Changed(ctx, Change{Node: "dk", Event: "push", Repository: "alice/new"})
		if strings.Join(sc.scanned, ",") != "alice/new" || *assigned != 1 || strings.Join(rp.triggered, ",") != "id-new" {
			t.Errorf("scanned %v, assigned %d, triggered %v", sc.scanned, *assigned, rp.triggered)
		}
	})

	t.Run("a push under a new name checks the old name too, so the rename is found", func(t *testing.T) {
		d, lk, sc, rp, _ := newD()
		lk.prev["alice/app"] = "alice/demo"
		sc.onScan = func(name string) {
			if name == "alice/demo" { // the rename gets merged: the new name maps to the old record
				lk.ids["alice/app"] = "id-1"
			}
		}
		d.Changed(ctx, Change{Node: "se", Event: "push", Repository: "alice/app"})
		if strings.Join(sc.scanned, ",") != "alice/app,alice/demo" || strings.Join(rp.triggered, ",") != "id-1" {
			t.Errorf("scanned %v, triggered %v", sc.scanned, rp.triggered)
		}
	})

	t.Run("repository events are looked at even when known", func(t *testing.T) {
		d, _, sc, rp, _ := newD()
		d.Changed(ctx, Change{Node: "se", Event: "repository", Action: "deleted", Repository: "alice/demo"})
		if strings.Join(sc.scanned, ",") != "alice/demo" || strings.Join(rp.triggered, ",") != "id-1" {
			t.Errorf("scanned %v, triggered %v", sc.scanned, rp.triggered)
		}
	})

	t.Run("without replication only the inventory is refreshed", func(t *testing.T) {
		d, _, sc, _, _ := newD()
		d.Replicator = nil
		d.Changed(ctx, Change{Node: "se", Event: "repository", Repository: "alice/demo"})
		if len(sc.scanned) != 1 {
			t.Errorf("scanned %v", sc.scanned)
		}
	})
}

func TestHookURLChangesWithSecret(t *testing.T) {
	a := HookURL("http://x/hooks/", "se", NodeSecret(master, "se"))
	b := HookURL("http://x/hooks", "se", NodeSecret(strings.Repeat("z", 32), "se"))
	if !strings.HasPrefix(a, "http://x/hooks/se?v=") || a == b || NodeSecret(master, "se") == NodeSecret(master, "dk") {
		t.Errorf("%s / %s", a, b)
	}
}

var _ http.Handler = (*Receiver)(nil)

type fakeIssues struct{ triggered []string }

func (f *fakeIssues) Trigger(_ context.Context, id string) { f.triggered = append(f.triggered, id) }

func TestDispatcherIssueEvents(t *testing.T) {
	lk := &fakeLookup{ids: map[string]string{"alice/demo": "id-1"}, prev: map[string]string{}}
	sc, rp, is := &fakeScanner{}, &fakeReplicator{}, &fakeIssues{}
	d := &RepoDispatcher{Store: lk, Scanner: sc, Replicator: rp, Issues: is, Log: slog.New(slog.DiscardHandler)}
	d.Changed(context.Background(), Change{Node: "dk", Event: "issue_comment", Repository: "alice/demo"})
	d.Changed(context.Background(), Change{Node: "dk", Event: "issues", Repository: "alice/unknown"})
	if strings.Join(is.triggered, ",") != "id-1" || len(rp.triggered) != 0 || len(sc.scanned) != 0 {
		t.Errorf("issues %v, git %v, scanned %v", is.triggered, rp.triggered, sc.scanned)
	}
}
