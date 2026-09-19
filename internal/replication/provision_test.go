package replication

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// fakeAPI is a node's REST API in front of a gitNode: creating a repository
// makes a real bare repository the engine can push to.
type fakeAPI struct {
	mu        sync.Mutex
	git       *gitNode
	users     map[string]forgejo.User
	orgs      map[string]bool
	orgOwners map[string][]string
	repos     map[string]forgejo.Repository
	pulls     []fakePull
	comments  []string
	calls     []string
	// failTransfer makes the next TransferRepo fail.
	failTransfer bool
}

// fakePull is a pull request as this fake keeps it: what it was opened
// with, and what a reader gets back. They are separate types with the same
// field names, so the result is held under a name of its own.
type fakePull struct {
	forgejo.CreatePullRequestOption
	pr forgejo.PullRequest
}

func newFakeAPI(g *gitNode) *fakeAPI {
	return &fakeAPI{git: g, users: map[string]forgejo.User{}, orgs: map[string]bool{}, repos: map[string]forgejo.Repository{}}
}

func (f *fakeAPI) GetUser(_ context.Context, login string) (forgejo.User, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[login]
	return u, ok, nil
}
func (f *fakeAPI) IsOrg(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orgs[name], nil
}
func (f *fakeAPI) GetRepo(_ context.Context, owner, name string) (forgejo.Repository, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[owner+"/"+name]
	return r, ok, nil
}
func (f *fakeAPI) UsersByLoginName(_ context.Context, src int64, login string) ([]forgejo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgejo.User
	for _, u := range f.users {
		if u.SourceID == src && u.LoginName == login {
			out = append(out, u)
		}
	}
	return out, nil
}
func (f *fakeAPI) AdminCreateUser(_ context.Context, opt forgejo.CreateUserOption) (forgejo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Email == opt.Email {
			return forgejo.User{}, &forgejo.APIError{StatusCode: http.StatusUnprocessableEntity, Message: "e-mail already used"}
		}
	}
	u := forgejo.User{Login: opt.Username, Email: opt.Email, FullName: opt.FullName, SourceID: opt.SourceID, LoginName: opt.LoginName}
	f.users[opt.Username] = u
	f.calls = append(f.calls, "create user "+opt.Username)
	return u, nil
}
func (f *fakeAPI) AdminCreateRepo(_ context.Context, owner string, opt forgejo.CreateRepoOption) (forgejo.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[owner]; !ok && !f.orgs[owner] {
		return forgejo.Repository{}, &forgejo.APIError{StatusCode: http.StatusNotFound}
	}
	r := forgejo.Repository{FullName: owner + "/" + opt.Name, Name: opt.Name, Owner: forgejo.User{Login: owner},
		Private: opt.Private, DefaultBranch: opt.DefaultBranch}
	f.repos[r.FullName] = r
	f.git.create(r.FullName)
	f.calls = append(f.calls, "create repo "+r.FullName)
	return r, nil
}

func (f *fakeAPI) SetDefaultBranch(_ context.Context, owner, repo, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	r.DefaultBranch = branch
	f.repos[owner+"/"+repo] = r
	f.calls = append(f.calls, "default branch "+owner+"/"+repo+" "+branch)
	return nil
}
func (f *fakeAPI) CreatePullRequest(_ context.Context, owner, repo string, opt forgejo.CreatePullRequestOption) (forgejo.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int64(len(f.pulls) + 1)
	pr := forgejo.PullRequest{Number: n, State: "open", HTMLURL: fmt.Sprintf("http://se/%s/%s/pulls/%d", owner, repo, n)}
	f.pulls = append(f.pulls, fakePull{opt, pr})
	return pr, nil
}
func (f *fakeAPI) GetPullRequest(_ context.Context, _, _ string, n int64) (forgejo.PullRequest, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 1 || int(n) > len(f.pulls) {
		return forgejo.PullRequest{}, false, nil
	}
	return f.pulls[n-1].pr, true, nil
}
func (f *fakeAPI) Comment(_ context.Context, _, _ string, n int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, fmt.Sprintf("#%d %s", n, body))
	return nil
}
func (f *fakeAPI) has(full string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[full]
	return ok
}
func (f *fakeAPI) moveRepo(from, to string) {
	r := f.repos[from]
	delete(f.repos, from)
	owner, name, _ := strings.Cut(to, "/")
	r.FullName, r.Name, r.Owner.Login = to, name, owner
	f.repos[to] = r
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.git.root, to)), 0o755); err != nil {
		panic(err)
	}
	if err := os.Rename(filepath.Join(f.git.root, from+".git"), filepath.Join(f.git.root, to+".git")); err != nil {
		panic(err)
	}
}
func (f *fakeAPI) EditRepo(_ context.Context, owner, repo string, opt forgejo.EditRepoOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := owner + "/" + repo
	if _, ok := f.repos[full]; !ok {
		return &forgejo.APIError{StatusCode: http.StatusNotFound}
	}
	if opt.Name != nil {
		f.moveRepo(full, owner+"/"+*opt.Name)
		f.calls = append(f.calls, "rename "+full+" "+*opt.Name)
	}
	if opt.Archived != nil {
		f.calls = append(f.calls, "archive "+full)
		if r, ok := f.repos[full]; ok {
			r.Archived = *opt.Archived
			f.repos[full] = r
		}
	}
	return nil
}
func (f *fakeAPI) TransferRepo(_ context.Context, owner, repo, newOwner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTransfer {
		f.failTransfer = false
		return &forgejo.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	}
	f.moveRepo(owner+"/"+repo, newOwner+"/"+repo)
	f.calls = append(f.calls, "transfer "+owner+"/"+repo+" "+newOwner)
	return nil
}
func (f *fakeAPI) DeleteRepo(_ context.Context, owner, repo string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := owner + "/" + repo
	if _, ok := f.repos[full]; ok {
		delete(f.repos, full)
		os.RemoveAll(filepath.Join(f.git.root, full+".git"))
		f.calls = append(f.calls, "delete "+full)
	}
	return nil
}
func (f *fakeAPI) AdminCreateOrg(_ context.Context, owner string, opt forgejo.CreateOrgOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgs[opt.UserName] = true
	f.calls = append(f.calls, "create org "+opt.UserName+" "+opt.Visibility+" for "+owner)
	return nil
}
func (f *fakeAPI) OrgOwners(_ context.Context, org string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.orgOwners[org], nil
}

const sub = "9115d51e-7245-44e0-ba34-f4c857ad98a3"

// setupCreate: alice/demo exists only on the primary se; dk doesn't have it.
// The SceneID source is id 1 on se and id 3 on dk.
func setupCreate(t *testing.T) (*Engine, *memStore, *gitNode, *gitNode, *fakeAPI, *fakeAPI, *workTree) {
	t.Helper()
	se, dk := newGitNode(t), newGitNode(t)
	se.create("alice/demo")
	seAPI, dkAPI := newFakeAPI(se), newFakeAPI(dk)
	seAPI.users["alice"] = forgejo.User{Login: "alice", Email: "alice@example.org", SourceID: 1, LoginName: sub}
	seAPI.repos["alice/demo"] = forgejo.Repository{FullName: "alice/demo", Name: "demo", Owner: forgejo.User{Login: "alice"},
		Private: true, DefaultBranch: "main"}
	st := newMemStore(store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"})
	h := fixedHealth{"se": health.Healthy, "dk": health.Healthy}
	e := NewEngine([]Node{
		{Name: "se", URL: se.srv.URL, User: testUser, Token: testToken, API: seAPI, SceneIDSourceID: 1},
		{Name: "dk", URL: dk.srv.URL, User: testUser, Token: testToken, API: dkAPI, SceneIDSourceID: 3},
	}, testGit(t), st, h, Options{CreateMissing: true}, slog.New(slog.DiscardHandler))
	return e, st, se, dk, seAPI, dkAPI, newWorkTree(t)
}

func TestCreateMissingRepository(t *testing.T) {
	ctx := context.Background()

	t.Run("creates the SceneID owner and the repository, then replicates", func(t *testing.T) {
		e, st, se, dk, _, dkAPI, w := setupCreate(t)
		b := w.commit("b")
		w.push(se, "alice/demo", "main")
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		if s := st.sync("dk"); s.State != StateSynced || dk.refs("alice/demo")["refs/heads/main"] != b {
			t.Fatalf("state %+v, dk refs %v", s, dk.refs("alice/demo"))
		}
		u := dkAPI.users["alice"]
		if u.SourceID != 3 || u.LoginName != sub {
			t.Errorf("alice on dk = %+v; want dk's SceneID source (3) and the same sub", u)
		}
		if r := dkAPI.repos["alice/demo"]; !r.Private || r.DefaultBranch != "main" {
			t.Errorf("repo on dk = %+v", r)
		}
		if strings.Join(st.audit, ",") != "noted dk/alice,forgesync user.created_on_node,forgesync repo.created_on_node" {
			t.Errorf("audit = %v", st.audit)
		}
		// Next run: nothing more to create.
		e.RunRepo(ctx, st.rec)
		if len(dkAPI.calls) != 2 {
			t.Errorf("calls = %v", dkAPI.calls)
		}
	})

	t.Run("existing owner with the same sub is reused", func(t *testing.T) {
		e, st, se, _, _, dkAPI, w := setupCreate(t)
		dkAPI.users["alice"] = forgejo.User{Login: "alice", Email: "alice@example.org", SourceID: 3, LoginName: sub}
		w.commit("a")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateSynced || strings.Join(dkAPI.calls, ",") != "create repo alice/demo" {
			t.Errorf("state %+v, calls %v", s, dkAPI.calls)
		}
	})

	blockedCases := []struct {
		name    string
		prepare func(se, dk *fakeAPI)
		want    string
	}{
		{"a different account holds the name on the replica", func(_, dk *fakeAPI) {
			dk.users["alice"] = forgejo.User{Login: "alice", SourceID: 3, LoginName: "someone-else"}
		}, "belongs to a different account"},
		{"a local account holds the name on the replica", func(_, dk *fakeAPI) {
			dk.users["alice"] = forgejo.User{Login: "alice"}
		}, "belongs to a different account"},
		{"the same person has another name on the replica", func(_, dk *fakeAPI) {
			dk.users["alice2"] = forgejo.User{Login: "alice2", SourceID: 3, LoginName: sub}
		}, "is alice2 on dk"},
		{"a local owner without a local account on the replica", func(se, _ *fakeAPI) {
			se.users["alice"] = forgejo.User{Login: "alice", Email: "alice@example.org"}
		}, "doesn't create local accounts"},
		{"the replica refuses the account", func(_, dk *fakeAPI) {
			dk.users["other"] = forgejo.User{Login: "other", Email: "alice@example.org", SourceID: 3, LoginName: "x"}
		}, "refused to create the owner"},
		{"pull mirrors aren't created", func(se, _ *fakeAPI) {
			r := se.repos["alice/demo"]
			r.Mirror = true
			se.repos["alice/demo"] = r
		}, "pull mirror"},
		{"organizations must exist on the replica", func(se, _ *fakeAPI) {
			se.orgs["alice"] = true
		}, "doesn't create organizations"},
	}
	for _, c := range blockedCases {
		t.Run(c.name, func(t *testing.T) {
			e, st, se, dk, seAPI, dkAPI, w := setupCreate(t)
			c.prepare(seAPI, dkAPI)
			w.commit("a")
			w.push(se, "alice/demo", "main")
			if err := e.RunRepo(ctx, st.rec); err != nil {
				t.Fatal(err)
			}
			s := st.sync("dk")
			if s.State != StateMissing || !strings.Contains(s.Detail, c.want) {
				t.Errorf("state %+v, want missing with %q", s, c.want)
			}
			if _, err := os.Stat(filepath.Join(dk.root, "alice/demo.git")); err == nil {
				t.Error("the repository was created anyway")
			}
			if len(st.checked) != 0 {
				t.Error("an incomplete run must not clear conflicts")
			}
		})
	}

	t.Run("a local admin owner is matched by name", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, w := setupCreate(t)
		seAPI.users["alice"] = forgejo.User{Login: "alice", IsAdmin: true}
		dkAPI.users["alice"] = forgejo.User{Login: "alice", IsAdmin: true}
		w.commit("a")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateSynced || strings.Join(dkAPI.calls, ",") != "create repo alice/demo" {
			t.Errorf("state %+v, calls %v", s, dkAPI.calls)
		}
	})

	t.Run("an organization that exists on the replica is fine", func(t *testing.T) {
		e, st, se, _, seAPI, dkAPI, w := setupCreate(t)
		seAPI.orgs["alice"], dkAPI.orgs["alice"] = true, true
		w.commit("a")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateSynced || strings.Join(dkAPI.calls, ",") != "create repo alice/demo" {
			t.Errorf("state %+v, calls %v", s, dkAPI.calls)
		}
	})

	t.Run("a recreated replica starts from a clean base", func(t *testing.T) {
		e, st, se, dk, _, dkAPI, w := setupCreate(t)
		b := w.commit("b")
		w.push(se, "alice/demo", "main")
		e.RunRepo(ctx, st.rec)
		// Someone deletes the copy on dk; the next run recreates and refills it.
		os.RemoveAll(filepath.Join(dk.root, "alice/demo.git"))
		delete(dkAPI.repos, "alice/demo")
		e.RunRepo(ctx, st.rec)
		if s := st.sync("dk"); s.State != StateSynced || dk.refs("alice/demo")["refs/heads/main"] != b {
			t.Errorf("state %+v, refs %v", s, dk.refs("alice/demo"))
		}
	})
}

func TestSeedPrimary(t *testing.T) {
	ctx := context.Background()
	// alice's primary site is se, but she created alice/demo on dk: se gets
	// it from dk first, then replication carries on from se as usual.
	e, st, se, dk, seAPI, dkAPI, w := setupCreate(t)
	os.RemoveAll(filepath.Join(se.root, "alice/demo.git"))
	delete(seAPI.repos, "alice/demo")
	dk.create("alice/demo")
	dkAPI.users["alice"] = forgejo.User{Login: "alice", Email: "alice@example.org", SourceID: 3, LoginName: sub}
	dkAPI.repos["alice/demo"] = forgejo.Repository{FullName: "alice/demo", Name: "demo", Owner: forgejo.User{Login: "alice"},
		DefaultBranch: "main"}
	seAPI.users = map[string]forgejo.User{} // and she has no account on se yet
	created := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	st.rec.Replicas = []store.Replica{{Node: "dk", Present: true, ForgejoCreated: &created}}

	b := w.commit("b")
	runGit(t, w.dir, "tag", "v1")
	w.push(dk, "alice/demo", "main", "refs/tags/v1")
	if err := e.RunRepo(ctx, st.rec); err != nil {
		t.Fatal(err)
	}
	if got := se.refs("alice/demo"); got["refs/heads/main"] != b || got["refs/tags/v1"] != b {
		t.Fatalf("se (primary) = %v", got)
	}
	if u := seAPI.users["alice"]; u.SourceID != 1 || u.LoginName != sub {
		t.Errorf("alice on se = %+v", u)
	}
	if s := st.sync("dk"); s.State != StateSynced {
		t.Errorf("dk after seeding = %+v", s)
	}
	if !strings.Contains(strings.Join(st.audit, ","), "noted se/alice") {
		t.Errorf("created account not noted: %v", st.audit)
	}

	t.Run("no copy to start from", func(t *testing.T) {
		e, st, se, _, seAPI, _, _ := setupCreate(t)
		os.RemoveAll(filepath.Join(se.root, "alice/demo.git"))
		delete(seAPI.repos, "alice/demo")
		if err := e.RunRepo(ctx, st.rec); err != nil {
			t.Fatal(err)
		}
		if s := st.sync("dk"); s.State != StateMissing || !strings.Contains(s.Detail, "no healthy node has a copy") {
			t.Errorf("state = %+v", s)
		}
	})
}
