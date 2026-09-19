package replication

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	// collabs is who the repository is shared with here, and what they may
	// do; refusesGrant is a login this node won't take.
	collabs      map[string]string
	refusesGrant string
	// rules are this node's branch protection rules, by rule name, and
	// topics its repositories' topics.
	rules  map[string]forgejo.BranchProtection
	topics map[string][]string
	// releases are what this node has published, with the bytes of their
	// files; tags are the tags it has; name and as identify it and who it
	// is acting as.
	releases    map[string][]*forgejo.Release
	assetBytes  map[int64][]byte
	tags        []string
	nextRelease int64
	name        string
	as          string
	// orgState is each organization this node has, with its teams and who
	// is in them; orgs above is only what IsOrg answers.
	orgState map[string]*fakeOrg
	nextTeam int64
}

// fakeOrg is one organization on a node.
type fakeOrg struct {
	org     forgejo.Org
	teams   map[string]*forgejo.Team // by name
	members map[int64]map[string]bool
}

// meta is what this node's repository settings and topics are.
// releases and files are what this node has published, by repository.
func (f *fakeAPI) Releases(_ context.Context, owner, repo string, page, _ int) ([]forgejo.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if page > 1 {
		return nil, nil
	}
	var out []forgejo.Release
	for _, r := range f.releases[owner+"/"+repo] {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TagName < out[j].TagName })
	return out, nil
}

func (f *fakeAPI) CreateRelease(_ context.Context, owner, repo string, r forgejo.Release) (forgejo.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releases == nil {
		f.releases = map[string][]*forgejo.Release{}
	}
	f.nextRelease++
	r.ID = f.nextRelease
	r.Author = forgejo.User{Login: f.as}
	// Forgejo's create takes the words and the tag, nothing else: a new
	// release starts with no files.
	r.Assets = nil
	f.releases[owner+"/"+repo] = append(f.releases[owner+"/"+repo], &r)
	f.calls = append(f.calls, "publish "+r.TagName+" as "+f.as)
	return r, nil
}

func (f *fakeAPI) EditRelease(_ context.Context, owner, repo string, id int64, r forgejo.Release) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.releases[owner+"/"+repo] {
		if x.ID == id {
			x.Title, x.Body, x.Draft, x.Prerelease = r.Title, r.Body, r.Draft, r.Prerelease
			f.calls = append(f.calls, "edit release "+x.TagName)
			return nil
		}
	}
	return fmt.Errorf("no release %d", id)
}

func (f *fakeAPI) DeleteRelease(_ context.Context, owner, repo string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.releases[owner+"/"+repo]
	for i, x := range list {
		if x.ID == id {
			f.calls = append(f.calls, "unpublish "+x.TagName)
			f.releases[owner+"/"+repo] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeAPI) UploadReleaseAsset(_ context.Context, owner, repo string, release int64, name string, content []byte) (forgejo.ReleaseAsset, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.releases[owner+"/"+repo] {
		if x.ID != release {
			continue
		}
		f.nextRelease++
		a := forgejo.ReleaseAsset{ID: f.nextRelease, Name: name, Size: int64(len(content)),
			DownloadURL: fmt.Sprintf("http://%s/assets/%d", f.name, f.nextRelease)}
		x.Assets = append(x.Assets, a)
		if f.assetBytes == nil {
			f.assetBytes = map[int64][]byte{}
		}
		f.assetBytes[a.ID] = content
		f.calls = append(f.calls, "upload "+name)
		return a, nil
	}
	return forgejo.ReleaseAsset{}, fmt.Errorf("no release %d", release)
}

func (f *fakeAPI) DeleteReleaseAsset(_ context.Context, owner, repo string, release, asset int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.releases[owner+"/"+repo] {
		if x.ID != release {
			continue
		}
		for i, a := range x.Assets {
			if a.ID == asset {
				x.Assets = append(x.Assets[:i], x.Assets[i+1:]...)
				f.calls = append(f.calls, "delete asset "+a.Name)
				return nil
			}
		}
	}
	return nil
}

func (f *fakeAPI) Tags(_ context.Context, _, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tags...), nil
}

func (f *fakeAPI) Download(_ context.Context, url string, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, err := strconv.ParseInt(url[strings.LastIndex(url, "/")+1:], 10, 64)
	if err != nil {
		return nil, err
	}
	b, ok := f.assetBytes[id]
	if !ok {
		return nil, fmt.Errorf("no asset %d on %s", id, f.name)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return b, nil
}

func (f *fakeAPI) EditRepoFields(_ context.Context, owner, repo string, fields map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[owner+"/"+repo]
	for k, v := range fields {
		switch k {
		case "description":
			r.Description, _ = v.(string)
		case "website":
			r.Website, _ = v.(string)
		case "default_merge_style":
			r.DefaultMergeStyle, _ = v.(string)
		case "private":
			r.Private, _ = v.(bool)
		case "has_issues":
			r.HasIssues, _ = v.(bool)
		case "has_wiki":
			r.HasWiki, _ = v.(bool)
		case "has_projects":
			r.HasProjects, _ = v.(bool)
		case "has_pull_requests":
			r.HasPullRequests, _ = v.(bool)
		case "has_actions":
			r.HasActions, _ = v.(bool)
		case "allow_merge_commits":
			r.AllowMergeCommits, _ = v.(bool)
		case "allow_rebase":
			r.AllowRebase, _ = v.(bool)
		case "allow_rebase_explicit":
			r.AllowRebaseExplicit, _ = v.(bool)
		case "allow_squash_merge":
			r.AllowSquashMerge, _ = v.(bool)
		case "delete_branch_after_merge":
			r.DeleteBranchAfterMerge, _ = v.(bool)
		}
		f.calls = append(f.calls, "set "+k)
	}
	f.repos[owner+"/"+repo] = r
	return nil
}

func (f *fakeAPI) Topics(_ context.Context, owner, repo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.topics[owner+"/"+repo]...), nil
}

func (f *fakeAPI) SetTopics(_ context.Context, owner, repo string, topics []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.topics == nil {
		f.topics = map[string][]string{}
	}
	f.topics[owner+"/"+repo] = append([]string(nil), topics...)
	f.calls = append(f.calls, "set topics")
	return nil
}

func (f *fakeAPI) BranchProtections(_ context.Context, _, _ string) ([]forgejo.BranchProtection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgejo.BranchProtection
	for _, r := range f.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RuleName < out[j].RuleName })
	return out, nil
}

func (f *fakeAPI) CreateBranchProtection(_ context.Context, _, _ string, r forgejo.BranchProtection) (forgejo.BranchProtection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rules == nil {
		f.rules = map[string]forgejo.BranchProtection{}
	}
	f.rules[r.RuleName] = r
	f.calls = append(f.calls, "protect "+r.RuleName)
	return r, nil
}

func (f *fakeAPI) EditBranchProtection(_ context.Context, _, _, rule string, r forgejo.BranchProtection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.RuleName = rule
	f.rules[rule] = r
	f.calls = append(f.calls, "edit protection "+rule)
	return nil
}

func (f *fakeAPI) DeleteBranchProtection(_ context.Context, _, _, rule string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, rule)
	f.calls = append(f.calls, "unprotect "+rule)
	return nil
}

func (f *fakeAPI) GetOrg(_ context.Context, name string) (forgejo.Org, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orgState[name]
	if o == nil {
		return forgejo.Org{}, false, nil
	}
	return o.org, true, nil
}

func (f *fakeAPI) EditOrg(_ context.Context, name string, fields map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orgState[name]
	if o == nil {
		return fmt.Errorf("no organization %s", name)
	}
	for k, v := range fields {
		s, _ := v.(string)
		switch k {
		case "full_name":
			o.org.FullName = s
		case "description":
			o.org.Description = s
		case "website":
			o.org.Website = s
		case "location":
			o.org.Location = s
		case "visibility":
			o.org.Visibility = s
		}
	}
	f.calls = append(f.calls, "edit org "+name)
	return nil
}

func (f *fakeAPI) OrgTeams(_ context.Context, org string) ([]forgejo.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orgState[org]
	if o == nil {
		return nil, nil
	}
	var out []forgejo.Team
	for _, t := range o.teams {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeAPI) CreateTeam(_ context.Context, org string, t forgejo.Team) (forgejo.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orgState[org]
	if o == nil {
		return forgejo.Team{}, fmt.Errorf("no organization %s", org)
	}
	f.nextTeam++
	t.ID = f.nextTeam
	o.teams[t.Name] = &t
	o.members[t.ID] = map[string]bool{}
	f.calls = append(f.calls, "create team "+t.Name)
	return t, nil
}

func (f *fakeAPI) DeleteTeam(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orgState {
		for name, t := range o.teams {
			if t.ID == id {
				delete(o.teams, name)
				delete(o.members, id)
				f.calls = append(f.calls, "delete team "+name)
				return nil
			}
		}
	}
	return nil
}

func (f *fakeAPI) TeamMembers(_ context.Context, id int64) ([]forgejo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgejo.User
	for _, o := range f.orgState {
		for login := range o.members[id] {
			out = append(out, forgejo.User{Login: login})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out, nil
}

func (f *fakeAPI) AddTeamMember(_ context.Context, id int64, login string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orgState {
		if o.members[id] != nil {
			o.members[id][login] = true
			f.calls = append(f.calls, "add member "+login)
			return nil
		}
	}
	return fmt.Errorf("no team %d", id)
}

func (f *fakeAPI) RemoveTeamMember(_ context.Context, id int64, login string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orgState {
		delete(o.members[id], login)
	}
	f.calls = append(f.calls, "remove member "+login)
	return nil
}

// fakePull is a pull request as this fake keeps it: what it was opened
// with, and what a reader gets back. They are separate types with the same
// field names, so the result is held under a name of its own.
type fakePull struct {
	forgejo.CreatePullRequestOption
	pr forgejo.PullRequest
}

// collaborators the node has, as "<login>:<permission>". refusesGrant is a
// login it won't take, standing in for one ForgeSync can't create there.
func (f *fakeAPI) Collaborators(_ context.Context, _, _ string) ([]forgejo.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forgejo.User
	for login := range f.collabs {
		out = append(out, forgejo.User{Login: login})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out, nil
}

func (f *fakeAPI) CollaboratorPermission(_ context.Context, _, _, login string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.collabs[login], nil
}

func (f *fakeAPI) AddCollaborator(_ context.Context, _, _, login, permission string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if login == f.refusesGrant {
		return fmt.Errorf("%s can't be given access here", login)
	}
	if f.collabs == nil {
		f.collabs = map[string]string{}
	}
	f.collabs[login] = permission
	f.calls = append(f.calls, "grant "+login+" "+permission)
	return nil
}

func (f *fakeAPI) RemoveCollaborator(_ context.Context, _, _, login string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.collabs, login)
	f.calls = append(f.calls, "ungrant "+login)
	return nil
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
	// As Forgejo does: the new organization has an Owners team with the
	// person it was created for in it.
	if f.orgState != nil {
		f.nextTeam++
		id := f.nextTeam
		f.orgState[opt.UserName] = &fakeOrg{
			org: forgejo.Org{Name: opt.UserName, FullName: opt.FullName, Description: opt.Description,
				Website: opt.Website, Location: opt.Location, Visibility: opt.Visibility},
			teams:   map[string]*forgejo.Team{"Owners": {ID: id, Name: "Owners", Permission: "owner"}},
			members: map[int64]map[string]bool{id: {owner: true}},
		}
	}
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
