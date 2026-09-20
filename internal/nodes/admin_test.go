package nodes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// A Forgejo that answers however the test needs it to.
type fakeNode struct {
	version    string
	versionErr error
	user       forgejo.User
	userErr    error
}

func (f fakeNode) Version(context.Context) (string, error) { return f.version, f.versionErr }
func (f fakeNode) CurrentUser(context.Context) (forgejo.User, error) {
	return f.user, f.userErr
}

func admin(t *testing.T, node fakeNode) (*Admin, *fake) {
	t.Helper()
	db := &fake{}
	return &Admin{DB: db, Key: testKey(t), Dial: func(string, string) (NodeAPI, error) { return node, nil }}, db
}

func good() NewNode {
	return NewNode{Name: "se", URL: "https://forgejo-se.example.org", Site: "SE",
		ServiceUser: "forgesync", SceneIDSourceID: 2, Token: "a-token"}
}

func findings(rep Report) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range rep.Findings {
		out[f.Check] = f
	}
	return out
}

func TestAddSealsTheTokenAndRecordsWhoAddedIt(t *testing.T) {
	a, db := admin(t, fakeNode{version: "16.0.1", user: forgejo.User{Login: "forgesync", IsAdmin: true}})
	rep, err := a.Add(context.Background(), good(), "account:khav")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("a good node was refused: %+v", rep.Findings)
	}
	if len(db.saved) != 1 {
		t.Fatalf("saved %d nodes", len(db.saved))
	}
	got := db.saved[0]
	if got.Source != "api" || got.AddedBy != "account:khav" {
		t.Errorf("source %q by %q", got.Source, got.AddedBy)
	}
	if len(got.SealedToken) == 0 {
		t.Fatal("the token was not sealed")
	}
	if strings.Contains(string(got.SealedToken), "a-token") {
		t.Fatal("the sealed token contains the token")
	}
	opened, err := testKey(t).Open(got.SealedToken)
	if err != nil || opened != "a-token" {
		t.Fatalf("the sealed token opened to %q, %v", opened, err)
	}
}

// A node ForgeSync cannot use must not be written down: the failure
// belongs where somebody is looking at it, not in the next round.
func TestABadNodeIsNotSaved(t *testing.T) {
	cases := map[string]fakeNode{
		"the node does not answer": {versionErr: errors.New("connection refused")},
		"the token does not work":  {version: "16.0.1", userErr: errors.New("401 Unauthorized")},
		"the account is not admin": {version: "16.0.1", user: forgejo.User{Login: "forgesync"}},
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := admin(t, node)
			rep, err := a.Add(context.Background(), good(), "account:khav")
			if err != nil {
				t.Fatal(err)
			}
			if rep.OK {
				t.Fatal("it was accepted")
			}
			if len(db.saved) != 0 {
				t.Fatalf("it was saved anyway: %+v", db.saved)
			}
		})
	}
}

// Everything is reported, not just the first thing wrong: somebody
// setting a node up wants the whole list.
func TestCheckReportsEverythingItLookedAt(t *testing.T) {
	a, _ := admin(t, fakeNode{version: "16.0.1", user: forgejo.User{Login: "forgesync", IsAdmin: true}})
	n := good()
	n.SceneIDSourceID = 0
	rep, err := a.Check(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	f := findings(rep)
	for _, want := range []string{"The node answers", "The token works", "That account is a site admin",
		"The account is the one you named", "The SceneID login source is named"} {
		if _, ok := f[want]; !ok {
			t.Errorf("nothing said about %q", want)
		}
	}
	// No SceneID source is a real limit, not a broken node.
	if s := f["The SceneID login source is named"]; s.OK || s.Blocking {
		t.Errorf("a missing SceneID source was ok=%v blocking=%v, want false and false", s.OK, s.Blocking)
	}
	if !rep.OK {
		t.Error("the node was refused for something that should not block")
	}
}

// The node knows who the token belongs to better than the person typing.
func TestTheNodeDecidesWhoTheTokenIs(t *testing.T) {
	a, db := admin(t, fakeNode{version: "16.0.1", user: forgejo.User{Login: "sync-bot", IsAdmin: true}})
	rep, err := a.Add(context.Background(), good(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("refused: %+v", rep.Findings)
	}
	if db.saved[0].ServiceUser != "sync-bot" {
		t.Errorf("stored service user %q, want the one the node named", db.saved[0].ServiceUser)
	}
	if f := findings(rep)["The account is the one you named"]; f.OK || f.Blocking {
		t.Errorf("the mismatch was ok=%v blocking=%v, want a note that does not block", f.OK, f.Blocking)
	}
}

func TestWithoutAKeyNothingIsAdded(t *testing.T) {
	db := &fake{}
	a := &Admin{DB: db, Dial: func(string, string) (NodeAPI, error) {
		return fakeNode{version: "16.0.1", user: forgejo.User{Login: "forgesync", IsAdmin: true}}, nil
	}}
	if _, err := a.Add(context.Background(), good(), "account:khav"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("got %v, want ErrNoKey", err)
	}
	if len(db.saved) != 0 {
		t.Fatal("a node was saved with no key to seal its token")
	}
}

func TestValidateRefusesWhatCannotWork(t *testing.T) {
	for name, n := range map[string]NewNode{
		"no name":            {URL: "https://x.example.org", Token: "t"},
		"a name with spaces": {Name: "two words", URL: "https://x.example.org", Token: "t"},
		"a name in capitals": {Name: "SE-Node!", URL: "https://x.example.org", Token: "t"},
		"no url":             {Name: "se", Token: "t"},
		"a relative url":     {Name: "se", URL: "forgejo-se", Token: "t"},
		"no token":           {Name: "se", URL: "https://x.example.org"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := n.Validate(); err == nil {
				t.Fatalf("%+v was accepted", n)
			}
		})
	}
	n := NewNode{Name: "  SE  ", URL: "https://x.example.org/", Token: "t"}
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
	if n.Name != "se" || n.URL != "https://x.example.org" || n.ServiceUser != "forgesync" {
		t.Errorf("tidied to %+v", n)
	}
}

var _ = store.NodeRecord{}
