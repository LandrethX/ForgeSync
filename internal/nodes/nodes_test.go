package nodes

import (
	"context"
	"errors"
	"testing"

	"scenegit.org/forgesync/internal/config"
	"scenegit.org/forgesync/internal/secret"
	"scenegit.org/forgesync/internal/store"
)

// A database of nodes, without a database.
type fake struct {
	rows  []store.NodeRecord
	saved []store.NodeRecord
}

func (f *fake) Nodes(context.Context) ([]store.NodeRecord, error) { return f.rows, nil }
func (f *fake) SaveNode(_ context.Context, n store.NodeRecord) error {
	f.saved = append(f.saved, n)
	f.rows = append(f.rows, n)
	return nil
}

func testKey(t *testing.T) *secret.Key {
	t.Helper()
	k, err := secret.NewKey(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func configured(names ...string) []config.Node {
	var out []config.Node
	for _, n := range names {
		out = append(out, config.Node{Name: n, URL: "http://" + n, Site: n, ServiceUser: "forgesync", Token: "tok-" + n})
	}
	return out
}

// The case every existing installation is in: nodes in the file, nothing
// in the database. They are taken across, and nothing else changes.
func TestFirstStartTakesTheConfigNodesAcross(t *testing.T) {
	f := &fake{}
	got, err := Resolve(context.Background(), f, nil, configured("se", "dk"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}
	for _, n := range got {
		if n.Token != "tok-"+n.Name {
			t.Errorf("%s: token %q", n.Name, n.Token)
		}
		if n.FromDatabase {
			t.Errorf("%s: says its token came from the database", n.Name)
		}
	}
	if len(f.saved) != 2 {
		t.Fatalf("saved %d nodes, want 2", len(f.saved))
	}
	for _, rec := range f.saved {
		if rec.SealedToken != nil {
			t.Errorf("%s: a token was sealed without a key", rec.Name)
		}
		if rec.Source != "config" {
			t.Errorf("%s: source is %q", rec.Name, rec.Source)
		}
	}
}

// A node added through the UI carries its own sealed token and needs no
// entry in anybody's config file.
func TestASealedNodeNeedsNoConfigEntry(t *testing.T) {
	key := testKey(t)
	sealed, err := key.Seal("tok-us")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{rows: []store.NodeRecord{
		{Name: "us", URL: "http://us", Site: "US", ServiceUser: "forgesync", SceneIDSourceID: 4,
			SealedToken: sealed, Source: "api", AddedBy: "account:khav"},
	}}
	got, err := Resolve(context.Background(), f, key, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Token != "tok-us" || !got[0].FromDatabase {
		t.Fatalf("resolved %+v", got)
	}
	if got[0].SceneIDSourceID != 4 {
		t.Errorf("sceneid source id is %d", got[0].SceneIDSourceID)
	}
	if len(f.saved) != 0 {
		t.Error("resolving wrote to the database when it had nothing to add")
	}
}

// Running with fewer nodes than the installation has is how replication
// stops without anybody noticing, so every one of these has to be an
// error rather than a node quietly left out.
func TestAnUnusableNodeIsAnErrorNotAnOmission(t *testing.T) {
	key := testKey(t)
	sealed, err := key.Seal("tok-us")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("sealed but no key configured", func(t *testing.T) {
		f := &fake{rows: []store.NodeRecord{{Name: "us", SealedToken: sealed}}}
		if _, err := Resolve(context.Background(), f, nil, nil, nil); err == nil {
			t.Fatal("a sealed node was accepted with no key")
		}
	})
	t.Run("sealed with the wrong key", func(t *testing.T) {
		other, err := secret.NewKey(append(make([]byte, secret.KeySize-1), 9))
		if err != nil {
			t.Fatal(err)
		}
		f := &fake{rows: []store.NodeRecord{{Name: "us", SealedToken: sealed}}}
		_, err = Resolve(context.Background(), f, other, nil, nil)
		if !errors.Is(err, secret.ErrWrongKey) {
			t.Fatalf("got %v, want ErrWrongKey", err)
		}
	})
	t.Run("no token anywhere", func(t *testing.T) {
		f := &fake{rows: []store.NodeRecord{{Name: "us", URL: "http://us"}}}
		_, err := Resolve(context.Background(), f, key, nil, nil)
		if !errors.Is(err, ErrNoToken) {
			t.Fatalf("got %v, want ErrNoToken", err)
		}
	})
}

// A node still taking its token from a file follows what the file says,
// so correcting an address there keeps working.
func TestTheConfigStillCorrectsAnUnsealedNode(t *testing.T) {
	f := &fake{rows: []store.NodeRecord{
		{Name: "se", URL: "http://old", Site: "SE", ServiceUser: "forgesync"},
	}}
	cfg := configured("se")
	cfg[0].URL = "http://moved"
	got, err := Resolve(context.Background(), f, nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].URL != "http://moved" {
		t.Errorf("url is %q, want the config's", got[0].URL)
	}
}

// A node added to the config file later is taken across too, so the file
// does not stop working the moment the database knows about anything.
func TestAConfigNodeAddedLaterIsTakenAcross(t *testing.T) {
	f := &fake{rows: []store.NodeRecord{{Name: "se", URL: "http://se", ServiceUser: "forgesync"}}}
	got, err := Resolve(context.Background(), f, nil, configured("se", "dk"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got))
	}
	if len(f.saved) != 1 || f.saved[0].Name != "dk" {
		t.Fatalf("saved %+v, want just dk", f.saved)
	}
}
