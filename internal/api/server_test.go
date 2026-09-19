package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/leader"
	"scenegit.org/forgesync/internal/store"
)

// auditEntry is what the fake records for each Audit call.
type auditEntry struct {
	Actor, Action, Target string
	Details               map[string]any
}

type fakeDB struct {
	pingErr     error
	mu          sync.Mutex
	audit       []auditEntry
	repos       []store.RepositoryRecord
	scans       []store.NodeScan
	conflicts   []store.Conflict
	syncs       []store.ReplicaSync
	users       []store.UserRecord
	archives    []store.Archive
	issues      []store.IssueRecord
	comments    []store.CommentRecord
	pairs       []store.SourcePair
	controllers []store.ControllerRecord
	sessions    map[string]fakeSession
	chosen      store.LeadershipChoice
	// accounts are ForgeSync's own, with their passwords as given: the
	// fake doesn't hash, so a test can say what it means.
	accounts  []store.Account
	passwords map[string]string
}

func (f *fakeDB) Accounts(context.Context) ([]store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Account(nil), f.accounts...), nil
}

func (f *fakeDB) Account(_ context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return store.Account{}, store.ErrNotFound
}

func (f *fakeDB) CreateAccount(_ context.Context, username, password, fullName, role, by string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if strings.EqualFold(a.Username, username) {
			return store.Account{}, store.ErrUsernameTaken
		}
	}
	a := store.Account{ID: "acct-" + username, Username: username, FullName: fullName, Role: role,
		CreatedBy: by, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.accounts = append(f.accounts, a)
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[strings.ToLower(username)] = password
	return a, nil
}

func (f *fakeDB) UpdateAccount(_ context.Context, id, fullName, role string, disabled bool) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.accounts {
		if a.ID == id {
			f.accounts[i].FullName, f.accounts[i].Role, f.accounts[i].Disabled = fullName, role, disabled
			return f.accounts[i], nil
		}
	}
	return store.Account{}, store.ErrNotFound
}

func (f *fakeDB) SetAccountPassword(_ context.Context, id, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if a.ID == id {
			if f.passwords == nil {
				f.passwords = map[string]string{}
			}
			f.passwords[strings.ToLower(a.Username)] = password
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeDB) DeleteAccount(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.accounts {
		if a.ID == id {
			f.accounts = append(f.accounts[:i], f.accounts[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeDB) CheckPassword(_ context.Context, username, password string) (store.Account, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if strings.EqualFold(a.Username, username) && !a.Disabled &&
			f.passwords[strings.ToLower(username)] == password && password != "" {
			return a, true, nil
		}
	}
	return store.Account{}, false, nil
}

func (f *fakeDB) Controllers(context.Context) ([]store.ControllerRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.controllers, nil
}

// fakeSession is one row of the sessions table.
type fakeSession struct {
	identity []byte
	lastUsed time.Time
	expires  time.Time
}

func (f *fakeDB) CreateSession(_ context.Context, hash string, identity []byte, expires time.Time, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessions == nil {
		f.sessions = map[string]fakeSession{}
	}
	f.sessions[hash] = fakeSession{identity: identity, lastUsed: time.Now(), expires: expires}
	return nil
}

func (f *fakeDB) Session(_ context.Context, hash string, idle time.Duration, touch bool) ([]byte, time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[hash]
	if !ok || time.Now().After(s.expires) || time.Since(s.lastUsed) > idle {
		return nil, time.Time{}, false, nil
	}
	if touch {
		s.lastUsed = time.Now()
		f.sessions[hash] = s
	}
	return s.identity, s.expires, true, nil
}

func (f *fakeDB) DeleteSession(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, hash)
	return nil
}

func (f *fakeDB) Chosen(context.Context) (store.LeadershipChoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chosen, nil
}

func (f *fakeDB) Choose(_ context.Context, controller, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chosen = store.LeadershipChoice{Controller: controller, ChosenBy: by, ChosenAt: time.Now()}
	return nil
}

func (f *fakeDB) ClearChoice(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chosen = store.LeadershipChoice{}
	return nil
}

func (f *fakeDB) SourcePairs(context.Context) ([]store.SourcePair, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pairs, nil
}

func (f *fakeDB) Repositories(context.Context) ([]store.RepositoryRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.RepositoryRecord(nil), f.repos...), nil
}
func (f *fakeDB) Repository(_ context.Context, id string) (store.RepositoryRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.repos {
		if r.ID == id {
			return r, nil
		}
	}
	return store.RepositoryRecord{}, store.ErrNotFound
}
func (f *fakeDB) SetPrimary(_ context.Context, id, node string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.repos {
		if f.repos[i].ID == id {
			prev := f.repos[i].PrimaryNode
			f.repos[i].PrimaryNode = node
			return prev, nil
		}
	}
	return "", store.ErrNotFound
}
func (f *fakeDB) Users(context.Context) ([]store.UserRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.UserRecord(nil), f.users...), nil
}
func (f *fakeDB) User(_ context.Context, id string) (store.UserRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.UserRecord{}, store.ErrNotFound
}
func (f *fakeDB) SetUserHome(_ context.Context, id, node string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.users {
		if f.users[i].ID == id {
			prev := f.users[i].HomeNode
			f.users[i].HomeNode, f.users[i].HomeSource = node, "manual"
			return prev, nil
		}
	}
	return "", store.ErrNotFound
}
func (f *fakeDB) Issues(context.Context, string) ([]store.IssueRecord, []store.CommentRecord, error) {
	return f.issues, f.comments, nil
}
func (f *fakeDB) Archives(context.Context, string) ([]store.Archive, error) { return f.archives, nil }
func (f *fakeDB) NodeScans(context.Context) ([]store.NodeScan, error)       { return f.scans, nil }
func (f *fakeDB) Conflicts(_ context.Context, flt store.ConflictFilter) ([]store.Conflict, int, map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	counts := map[string]int{"open": 0, "cleared": 0}
	items := []store.Conflict{}
	for _, c := range f.conflicts {
		counts[c.State]++
		if (flt.State == "" || c.State == flt.State) && (flt.RepositoryID == "" || c.RepositoryID == flt.RepositoryID) {
			items = append(items, c)
		}
	}
	return items, len(items), counts, nil
}
func (f *fakeDB) ConflictByID(_ context.Context, id int64) (store.Conflict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conflicts {
		if c.ID == id {
			return c, nil
		}
	}
	return store.Conflict{}, store.ErrNotFound
}
func (f *fakeDB) AcknowledgeConflict(_ context.Context, id int64, actor, note string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.conflicts {
		if f.conflicts[i].ID == id {
			f.conflicts[i].AcknowledgedBy, f.conflicts[i].Note = actor, note
			return nil
		}
	}
	return store.ErrNotFound
}
func (f *fakeDB) ReplicaSyncs(_ context.Context, id string) ([]store.ReplicaSync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.ReplicaSync
	for _, x := range f.syncs {
		if x.RepositoryID == id {
			out = append(out, x)
		}
	}
	return out, nil
}
func (f *fakeDB) ReplicationCounts(context.Context) (map[string]int, error) {
	counts := map[string]int{}
	for _, x := range f.syncs {
		counts[x.State]++
	}
	return counts, nil
}
func (f *fakeDB) DismissConflict(_ context.Context, id int64, actor, note string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.conflicts {
		if f.conflicts[i].ID == id && f.conflicts[i].State == "open" {
			f.conflicts[i].State = "dismissed"
			f.conflicts[i].DismissedBy, f.conflicts[i].DismissedAt = actor, &at
			if note != "" {
				f.conflicts[i].Note = note
			}
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeDB) ReopenConflict(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.conflicts {
		if f.conflicts[i].ID == id && f.conflicts[i].State == "dismissed" {
			f.conflicts[i].State = "open"
			f.conflicts[i].DismissedBy, f.conflicts[i].DismissedAt = "", nil
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeDB) OpenConflicts(context.Context) (int, error) {
	n := 0
	for _, c := range f.conflicts {
		if c.State == "open" {
			n++
		}
	}
	return n, nil
}

func (f *fakeDB) Ping(context.Context) error { return f.pingErr }
func (f *fakeDB) Transitions(_ context.Context, node string, limit int) ([]store.Transition, error) {
	return []store.Transition{{ID: 1, Node: "se", From: health.Unknown, To: health.Healthy}}, nil
}
func (f *fakeDB) History(_ context.Context, flt store.EventFilter) ([]store.Event, string, error) {
	if flt.Cursor == "bad" {
		return nil, "", store.ErrBadCursor
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Event
	for i := len(f.audit) - 1; i >= 0; i-- {
		e := f.audit[i]
		if flt.Actor != "" && e.Actor != flt.Actor {
			continue
		}
		out = append(out, store.Event{ID: fmt.Sprintf("a%d", i+1), Category: strings.SplitN(e.Action, ".", 2)[0],
			Actor: e.Actor, Action: e.Action, Target: e.Target, Details: e.Details})
	}
	return out, "", nil
}
func (f *fakeDB) HistoryEach(ctx context.Context, flt store.EventFilter, max int, fn func(store.Event) error) error {
	events, _, _ := f.History(ctx, flt)
	for i, e := range events {
		if i >= max {
			break
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeDB) HistoryActors(context.Context) ([]string, error) { return []string{"forgesync"}, nil }
func (f *fakeDB) Audit(_ context.Context, actor, action, target string, details map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, auditEntry{Actor: actor, Action: action, Target: target, Details: details})
	return nil
}
func (f *fakeDB) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.audit {
		out = append(out, e.Actor+" "+e.Action)
	}
	return out
}

type fakeHealth struct {
	mu     sync.Mutex
	status []health.Status
	subs   []chan struct{}
}

func (f *fakeHealth) Snapshot() []health.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]health.Status(nil), f.status...)
}
func (f *fakeHealth) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}
func (f *fakeHealth) set(s health.Status) {
	f.mu.Lock()
	f.status = []health.Status{s}
	subs := f.subs
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type fixture struct {
	h      http.Handler
	db     *fakeDB
	health *fakeHealth
	srv    *Server
}

func newFixture(token string) *fixture {
	f := &fixture{
		db:     &fakeDB{},
		health: &fakeHealth{status: []health.Status{{Node: "se", State: health.Healthy, Version: "16.0.5"}}},
	}
	f.srv = &Server{
		AdminToken: token,
		Nodes: []NodeInfo{
			{Name: "dk", URL: "http://forgejo-dk.test:3002", Site: "DK"},
			{Name: "se", URL: "http://forgejo-se.test:3001", Site: "SE"},
		},
		Health:        f.health,
		Inventory:     &fakeInventory{},
		DB:            f.db,
		Log:           slog.New(slog.DiscardHandler),
		StartedAt:     time.Now(),
		SecureCookies: true,
		Frontend:      http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("frontend")) }),
	}
	f.h = f.srv.Handler()
	return f
}

type req struct {
	method, path, body, bearer string
	cookie                     *http.Cookie
	csrf                       bool
}

func (f *fixture) do(r req) *httptest.ResponseRecorder {
	if r.method == "" {
		r.method = http.MethodGet
	}
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	hr.RemoteAddr = "192.0.2.10:5555"
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.cookie != nil {
		hr.AddCookie(r.cookie)
	}
	if r.csrf {
		hr.Header.Set(csrfHeader, "1")
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, hr)
	return rec
}

func (f *fixture) signIn(t *testing.T, token string) *http.Cookie {
	t.Helper()
	rec := f.do(req{method: "POST", path: "/api/v1/session", body: `{"token":"` + token + `"}`, csrf: true})
	if rec.Code != 200 {
		t.Fatalf("sign in = %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestProbes(t *testing.T) {
	f := newFixture("")
	if rec := f.do(req{path: "/healthz"}); rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}
	if rec := f.do(req{path: "/readyz"}); rec.Code != 200 {
		t.Errorf("/readyz with database = %d", rec.Code)
	}
	f.db.pingErr = errors.New("down")
	if rec := f.do(req{path: "/readyz"}); rec.Code != 503 {
		t.Errorf("/readyz without database = %d", rec.Code)
	}
}

func TestBearerToken(t *testing.T) {
	f := newFixture("s3cret")
	for _, tc := range []struct {
		bearer string
		want   int
	}{{"", 401}, {"wrong", 401}, {"s3cret", 200}} {
		if rec := f.do(req{path: "/api/v1/nodes", bearer: tc.bearer}); rec.Code != tc.want {
			t.Errorf("bearer %q = %d, want %d", tc.bearer, rec.Code, tc.want)
		}
	}
	if rec := newFixture("").do(req{path: "/api/v1/nodes"}); rec.Code != 503 {
		t.Errorf("admin API with neither token nor SceneID = %d, want 503", rec.Code)
	}
}

func TestSessionSignInAndOut(t *testing.T) {
	f := newFixture("s3cret")

	if rec := f.do(req{method: "POST", path: "/api/v1/session", body: `{"token":"s3cret"}`}); rec.Code != 403 {
		t.Errorf("sign in without CSRF header = %d, want 403", rec.Code)
	}
	if rec := f.do(req{method: "POST", path: "/api/v1/session", body: `{"token":"nope"}`, csrf: true}); rec.Code != 401 {
		t.Errorf("sign in with a wrong token = %d, want 401", rec.Code)
	}

	c := f.signIn(t, "s3cret")
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Errorf("cookie attributes: %+v", c)
	}
	if rec := f.do(req{path: "/api/v1/nodes", cookie: c}); rec.Code != 200 {
		t.Errorf("GET with session = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/session", cookie: c}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"administrator"`) {
		t.Errorf("GET /session = %d %s", rec.Code, rec.Body)
	}

	if rec := f.do(req{method: "DELETE", path: "/api/v1/session", cookie: c}); rec.Code != 200 {
		t.Errorf("sign out = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/nodes", cookie: c}); rec.Code != 401 {
		t.Errorf("GET after sign out = %d, want 401", rec.Code)
	}

	want := []string{"web-token session.sign_in_failed", "web-token session.sign_in", "web-token session.sign_out"}
	if got := f.db.actions(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestSessionCookieNeedsCSRFHeaderForWrites(t *testing.T) {
	f := newFixture("s3cret")
	c := f.signIn(t, "s3cret")
	// No write endpoints exist yet, so test the middleware directly.
	h := f.srv.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(identity(r).Actor()))
	}))
	call := func(method string, csrf bool, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		} else {
			r.AddCookie(c)
		}
		if csrf {
			r.Header.Set(csrfHeader, "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := call("POST", false, ""); rec.Code != 403 {
		t.Errorf("cookie POST without CSRF header = %d, want 403", rec.Code)
	}
	if rec := call("POST", true, ""); rec.Code != 200 || rec.Body.String() != "web-token" {
		t.Errorf("cookie POST with CSRF header = %d %q", rec.Code, rec.Body)
	}
	if rec := call("POST", false, "s3cret"); rec.Code != 200 || rec.Body.String() != "token" {
		t.Errorf("bearer POST (no cookie, no CSRF risk) = %d %q", rec.Code, rec.Body)
	}
}

func TestSignInRateLimit(t *testing.T) {
	f := newFixture("s3cret")
	for i := 0; i < 10; i++ {
		f.do(req{method: "POST", path: "/api/v1/session", body: `{"token":"guess"}`, csrf: true})
	}
	rec := f.do(req{method: "POST", path: "/api/v1/session", body: `{"token":"s3cret"}`, csrf: true})
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("11th attempt = %d (Retry-After %q), want 429 even with the right token", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestListNodesAndOverview(t *testing.T) {
	f := newFixture("s3cret")
	rec := f.do(req{path: "/api/v1/nodes", bearer: "s3cret"})
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("headers: %v", rec.Header())
	}
	var nodes []Node
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	// Config order; a node the monitor hasn't checked is UNKNOWN.
	if len(nodes) != 2 || nodes[0].Name != "dk" || nodes[0].State != health.Unknown ||
		nodes[1].State != health.Healthy || nodes[1].Site != "SE" {
		t.Errorf("nodes = %+v", nodes)
	}

	if rec := f.do(req{path: "/api/v1/nodes/se", bearer: "s3cret"}); rec.Code != 200 {
		t.Errorf("GET /nodes/se = %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/nodes/xx", bearer: "s3cret"}); rec.Code != 404 {
		t.Errorf("GET /nodes/xx = %d", rec.Code)
	}

	var o Overview
	json.Unmarshal(f.do(req{path: "/api/v1/overview", bearer: "s3cret"}).Body.Bytes(), &o)
	if o.Nodes["total"] != 2 || o.Nodes["HEALTHY"] != 1 || o.Nodes["UNKNOWN"] != 1 || !o.Database.OK || o.Role != "single" {
		t.Errorf("overview = %+v", o)
	}
}

func TestQueryValidation(t *testing.T) {
	f := newFixture("s3cret")
	if rec := f.do(req{path: "/api/v1/history?limit=0", bearer: "s3cret"}); rec.Code != 400 {
		t.Errorf("limit=0 -> %d", rec.Code)
	}
	if rec := f.do(req{path: "/api/v1/transitions?limit=10", bearer: "s3cret"}); rec.Code != 200 {
		t.Errorf("transitions -> %d", rec.Code)
	}
}

func TestFrontendAndAPINotFound(t *testing.T) {
	f := newFixture("s3cret")
	if rec := f.do(req{path: "/nodes/se"}); rec.Body.String() != "frontend" {
		t.Errorf("/nodes/se should go to the frontend, got %d %q", rec.Code, rec.Body)
	}
	rec := f.do(req{path: "/api/v1/nope", bearer: "s3cret"})
	if rec.Code != 404 || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Errorf("/api/v1/nope = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestEventStream(t *testing.T) {
	f := newFixture("s3cret")
	srv := httptest.NewServer(f.h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}

	lines := bufio.NewScanner(resp.Body)
	nextData := func() string {
		for lines.Scan() {
			if d, ok := strings.CutPrefix(lines.Text(), "data: "); ok {
				return d
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return ""
	}
	if first := nextData(); !strings.Contains(first, `"state":"HEALTHY"`) {
		t.Fatalf("first event = %s", first)
	}
	f.health.set(health.Status{Node: "se", State: health.Unreachable})
	if next := nextData(); !strings.Contains(next, `"state":"UNREACHABLE"`) {
		t.Fatalf("event after change = %s", next)
	}
}

// Sessions live in the database the controllers share, so one made on
// one controller is a session on the other: a failover doesn't sign
// anyone out.
func TestASessionWorksOnEitherController(t *testing.T) {
	db := &fakeDB{}
	a := NewSessions(8*time.Hour, 30*time.Minute, db, nil)
	b := NewSessions(8*time.Hour, 30*time.Minute, db, nil)

	key, expires, err := a.Create(t.Context(), webTokenIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if expires.Before(time.Now()) {
		t.Fatalf("expires = %s", expires)
	}
	id, _, ok := b.Get(t.Context(), key)
	if !ok || id.Username != webTokenIdentity.Username || id.Role != webTokenIdentity.Role {
		t.Fatalf("the other controller saw %+v (ok=%v)", id, ok)
	}
	// Signing out on one signs out on both.
	b.Delete(t.Context(), key)
	if a.Valid(t.Context(), key) {
		t.Error("still signed in on the first controller")
	}
}

// The cookie's value isn't what's stored: a copy of the table is no use
// to anyone.
func TestSessionKeysAreStoredHashed(t *testing.T) {
	db := &fakeDB{}
	s := NewSessions(time.Hour, time.Hour, db, nil)
	key, _, err := s.Create(t.Context(), webTokenIdentity)
	if err != nil {
		t.Fatal(err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for hash := range db.sessions {
		if hash == key {
			t.Fatal("the cookie's own value was stored")
		}
	}
}

// fakeLeader is a controller's view of the election.
type fakeLeader struct{ st leader.State }

func (f *fakeLeader) State() leader.State { return f.st }

func TestAStandbyServesThePagesButRefusesWrites(t *testing.T) {
	f, admin := sessionAs(t, auth.Administrator)
	f.srv.Leader = &fakeLeader{leader.State{Leading: false, Name: "forgesync-a", URL: "http://a:8090"}}

	// Reading is the same on both controllers.
	if rec := f.do(req{path: "/api/v1/nodes", cookie: admin}); rec.Code != 200 {
		t.Errorf("GET nodes = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(req{path: "/api/v1/overview", cookie: admin})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"standby"`) ||
		!strings.Contains(rec.Body.String(), `"name":"forgesync-a"`) {
		t.Errorf("overview = %d %s", rec.Code, rec.Body)
	}

	// Anything that changes the installation belongs to the leader.
	for _, w := range []req{
		{method: "POST", path: "/api/v1/inventory/scan", cookie: admin, csrf: true},
		{method: "PUT", path: "/api/v1/repositories/11111111-1111-1111-1111-111111111111/primary",
			body: `{"node":"se"}`, cookie: admin, csrf: true},
	} {
		rec := f.do(w)
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "forgesync-a is in charge") {
			t.Errorf("%s %s = %d %s", w.method, w.path, rec.Code, rec.Body)
		}
	}
	if f.srv.Inventory.(*fakeInventory).triggered != 0 {
		t.Error("the standby started a scan")
	}

	// But what someone writes about a conflict is ForgeSync's own
	// bookkeeping in the shared database, so it works from either one:
	// being on the standby is no reason to be unable to say "we know".
	f.db.conflicts = []store.Conflict{{ID: 1, RepositoryID: "11111111-1111-1111-1111-111111111111",
		FullName: "alice/demo", State: "open"}}
	for _, w := range []req{
		{method: "POST", path: "/api/v1/conflicts/1/acknowledge", body: `{"note":"looking"}`, cookie: admin, csrf: true},
		{method: "POST", path: "/api/v1/conflicts/1/dismiss", body: `{}`, cookie: admin, csrf: true},
		{method: "POST", path: "/api/v1/conflicts/1/reopen", cookie: admin, csrf: true},
	} {
		if rec := f.do(w); rec.Code != 200 {
			t.Errorf("%s %s on a standby = %d %s", w.method, w.path, rec.Code, rec.Body)
		}
	}

	// Once it's leading, the same request goes through.
	f.srv.Leader = &fakeLeader{leader.State{Leading: true, Name: "forgesync-b"}}
	if rec := f.do(req{method: "POST", path: "/api/v1/inventory/scan", cookie: admin, csrf: true}); rec.Code != 202 {
		t.Errorf("as leader = %d %s", rec.Code, rec.Body)
	}
	rec = f.do(req{path: "/api/v1/overview", cookie: admin})
	if !strings.Contains(rec.Body.String(), `"role":"leader"`) {
		t.Errorf("overview = %s", rec.Body)
	}

	// A single controller says so and never refuses.
	f.srv.Leader = nil
	rec = f.do(req{path: "/api/v1/overview", cookie: admin})
	if !strings.Contains(rec.Body.String(), `"role":"single"`) || strings.Contains(rec.Body.String(), `"leader"`) {
		t.Errorf("single = %s", rec.Body)
	}
}
