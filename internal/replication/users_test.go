package replication

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// usersSetup is three nodes, each with its own SceneID login source, as
// they have in the test environment.
func usersSetup(t *testing.T) (*Engine, *memStore, map[string]*fakeAPI) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	st := newMemStore(store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo"})
	var nodes []Node
	for i, name := range []string{"se", "dk", "de"} {
		nodes = append(nodes, Node{Name: name, URL: "http://" + name, User: testUser, Token: testToken,
			API: apis[name], SceneIDSourceID: int64(i + 1)})
	}
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{}, slog.New(slog.DiscardHandler))
	return e, st, apis
}

func TestCreateUserMakesTheAccountOnEveryNode(t *testing.T) {
	e, st, apis := usersSetup(t)
	created, refused, err := e.CreateUser(context.Background(), NewUser{
		Login: "dave", Subject: "dave-sub", FullName: "Dave", Email: "dave@example.test", Home: "dk"})
	if err != nil || len(refused) != 0 {
		t.Fatalf("created=%v refused=%v err=%v", created, refused, err)
	}
	slices.Sort(created)
	if strings.Join(created, ",") != "de,dk,se" {
		t.Fatalf("created on %v", created)
	}
	for name, api := range apis {
		u, ok := api.users["dave"]
		if !ok {
			t.Fatalf("%s has no account", name)
		}
		// Linked by the SceneID subject, through that node's own login
		// source: that's what makes their first sign-in find it.
		if u.LoginName != "dave-sub" || u.SourceID != e.nodes[name].SceneIDSourceID {
			t.Errorf("%s: %+v", name, u)
		}
	}
	// Only the home node counts as where they registered; the others are
	// ForgeSync's doing and must never be taken for a registration.
	if got := strings.Join(st.createdAccounts, ","); got != "de:dave,se:dave" && got != "se:dave,de:dave" {
		t.Errorf("accounts recorded as ForgeSync's: %v", st.createdAccounts)
	}

	// Asking again changes nothing: the accounts are already theirs.
	for _, a := range apis {
		a.calls = nil
	}
	created, refused, err = e.CreateUser(context.Background(), NewUser{Login: "dave", Subject: "dave-sub", Home: "dk"})
	if err != nil || len(created) != 0 || len(refused) != 0 {
		t.Fatalf("a second run: created=%v refused=%v err=%v", created, refused, err)
	}
	for name, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("%s was written to again: %v", name, a.calls)
		}
	}
}

// A name that belongs to someone else on one node is refused there, with
// the reason, and the nodes where it's free still get the account.
func TestCreateUserWontJoinTwoPeople(t *testing.T) {
	e, _, apis := usersSetup(t)
	apis["de"].users["dave"] = forgejo.User{Login: "dave", SourceID: 3, LoginName: "someone-else"}

	created, refused, err := e.CreateUser(context.Background(), NewUser{Login: "dave", Subject: "dave-sub", Home: "se"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(created)
	if strings.Join(created, ",") != "dk,se" {
		t.Fatalf("created on %v", created)
	}
	if !strings.Contains(refused["de"], "different account") {
		t.Errorf("de refused with %q", refused["de"])
	}
	if got := apis["de"].users["dave"].LoginName; got != "someone-else" {
		t.Errorf("de's account was changed: %q", got)
	}
}

// The same person under another name on a node is refused too, rather
// than being given a second account there.
func TestCreateUserWontGiveSomeoneASecondAccount(t *testing.T) {
	e, _, apis := usersSetup(t)
	apis["dk"].users["davey"] = forgejo.User{Login: "davey", SourceID: 2, LoginName: "dave-sub"}

	_, refused, err := e.CreateUser(context.Background(), NewUser{Login: "dave", Subject: "dave-sub", Home: "se"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refused["dk"], "davey") {
		t.Errorf("dk refused with %q", refused["dk"])
	}
	if _, made := apis["dk"].users["dave"]; made {
		t.Error("a second account was made on dk")
	}
}

// ForgeSync's administrators administer the controllers, not the nodes,
// and the local accounts on a node belong to whoever runs it. Neither is
// ForgeSync's to create: asking for one changes nothing anywhere.
func TestLocalAdminAccountsAreNotForgeSyncsToCreate(t *testing.T) {
	e, _, apis := usersSetup(t)
	for _, name := range []string{"se", "dk", "de"} {
		// Every node has these, set up by whoever runs it.
		apis[name].users["siteadmin"] = forgejo.User{Login: "siteadmin", SourceID: 0, IsAdmin: true}
		apis[name].users["forgesync"] = forgejo.User{Login: "forgesync", SourceID: 0, IsAdmin: true}
		apis[name].calls = nil
	}
	for _, login := range []string{"siteadmin", "forgesync"} {
		created, refused, err := e.CreateUser(context.Background(), NewUser{Login: login, Subject: "some-sub", Home: "se"})
		if err != nil {
			t.Fatal(err)
		}
		if len(created) != 0 {
			t.Errorf("%s was created on %v", login, created)
		}
		for _, name := range []string{"se", "dk", "de"} {
			if !strings.Contains(refused[name], "local account") {
				t.Errorf("%s on %s refused with %q", login, name, refused[name])
			}
			if u := apis[name].users[login]; u.LoginName != "" || u.SourceID != 0 {
				t.Errorf("%s's account on %s was changed: %+v", login, name, u)
			}
		}
	}
	for name, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("%s was written to: %v", name, a.calls)
		}
	}
}

// Someone who has signed in somewhere is copied to the nodes that
// haven't got them, without waiting for a repository of theirs.
func TestProvisionUserFillsInTheOtherNodes(t *testing.T) {
	e, st, apis := usersSetup(t)
	apis["se"].users["erin"] = forgejo.User{Login: "erin", SourceID: 1, LoginName: "erin-sub", Email: "erin@example.test"}

	created, refused, err := e.ProvisionUser(context.Background(), "erin")
	if err != nil || len(refused) != 0 {
		t.Fatalf("created=%v refused=%v err=%v", created, refused, err)
	}
	if strings.Join(created, ",") != "de,dk" {
		t.Fatalf("created on %v", created)
	}
	for _, name := range []string{"dk", "de"} {
		if u := apis[name].users["erin"]; u.LoginName != "erin-sub" {
			t.Errorf("%s: %+v", name, u)
		}
	}
	// These are ForgeSync's accounts, so se stays where erin registered.
	if len(st.createdAccounts) != 2 {
		t.Errorf("accounts recorded as ForgeSync's: %v", st.createdAccounts)
	}
}

func TestProvisionUserNeedsSomeoneToCopy(t *testing.T) {
	e, _, _ := usersSetup(t)
	if _, _, err := e.ProvisionUser(context.Background(), "nobody"); err == nil {
		t.Fatal("copying a user nobody has should fail")
	}
}
