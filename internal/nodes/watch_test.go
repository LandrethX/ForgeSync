package nodes

import (
	"context"
	"errors"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/store"
)

type opener struct{ err error }

func (o opener) Open(sealed []byte) (string, error) {
	if o.err != nil {
		return "", o.err
	}
	return "token-" + string(sealed), nil
}

func TestFingerprintFollowsWhatAControllerWiresFrom(t *testing.T) {
	base := []Node{
		{Name: "se", URL: "http://se", Site: "SE", ServiceUser: "forgesync", SceneIDSourceID: 1, Token: "a"},
		{Name: "dk", URL: "http://dk", Site: "DK", ServiceUser: "forgesync", SceneIDSourceID: 1, Token: "b"},
	}
	want := Fingerprint(base)

	// The order a database happened to return them in is not a change.
	if got := Fingerprint([]Node{base[1], base[0]}); got != want {
		t.Error("reordering the nodes changed the fingerprint")
	}

	for name, change := range map[string]func([]Node) []Node{
		"a node added":     func(n []Node) []Node { return append(n, Node{Name: "de", URL: "http://de"}) },
		"a node removed":   func(n []Node) []Node { return n[:1] },
		"an address moved": func(n []Node) []Node { c := clone(n); c[0].URL = "http://se-new"; return c },
		"a token replaced": func(n []Node) []Node { c := clone(n); c[0].Token = "new"; return c },
		"a site renamed":   func(n []Node) []Node { c := clone(n); c[0].Site = "SE2"; return c },
		"a source id set":  func(n []Node) []Node { c := clone(n); c[0].SceneIDSourceID = 9; return c },
		"a service user":   func(n []Node) []Node { c := clone(n); c[0].ServiceUser = "other"; return c },
	} {
		t.Run(name, func(t *testing.T) {
			if got := Fingerprint(change(base)); got == want {
				t.Error("the fingerprint did not notice")
			}
		})
	}

	// The token is in the fingerprint but must not be recoverable from it.
	if fp := Fingerprint(base); len(fp) != 64 {
		t.Errorf("fingerprint is %d characters, want a hex sha256", len(fp))
	}
}

func clone(n []Node) []Node { c := make([]Node, len(n)); copy(c, n); return c }

func TestWatchReturnsOnlyWhenSomethingChanged(t *testing.T) {
	rows := []store.NodeRecord{{Name: "se", URL: "http://se", ServiceUser: "forgesync", SealedToken: []byte("x")}}
	db := &fake{rows: rows}
	was, err := fingerprintStored(rows, opener{})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing has changed: it waits.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { Watch(ctx, db, opener{}, was, 10*time.Millisecond, nil); close(done) }()
	select {
	case <-done:
		if ctx.Err() == nil {
			t.Fatal("it returned while the nodes were the same")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("it did not stop when the context ended")
	}

	// A node added: it returns.
	db2 := &fake{rows: append(clone2(rows), store.NodeRecord{Name: "dk", URL: "http://dk", SealedToken: []byte("y")})}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	done2 := make(chan struct{})
	go func() { Watch(ctx2, db2, opener{}, was, 10*time.Millisecond, nil); close(done2) }()
	select {
	case <-done2:
	case <-time.After(1 * time.Second):
		t.Fatal("it did not notice a node being added")
	}
}

// A token this controller cannot open is a real problem, but restarting
// would not fix it and would loop, so it waits instead.
func TestWatchDoesNotRestartOverAKeyItCannotUse(t *testing.T) {
	rows := []store.NodeRecord{{Name: "se", URL: "http://se", SealedToken: []byte("x")}}
	db := &fake{rows: rows}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		Watch(ctx, db, opener{err: errors.New("wrong key")}, "whatever", 10*time.Millisecond, nil)
		close(done)
	}()
	select {
	case <-done:
		if ctx.Err() == nil {
			t.Fatal("it asked for a restart over a key it cannot use")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("it did not stop when the context ended")
	}
}

func clone2(r []store.NodeRecord) []store.NodeRecord {
	c := make([]store.NodeRecord, len(r))
	copy(c, r)
	return c
}
