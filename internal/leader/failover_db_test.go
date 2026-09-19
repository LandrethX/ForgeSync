package leader

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"log/slog"

	"scenegit.org/forgesync/internal/store"
)

// Two controllers, one database, real clocks: this is the failover the
// installation actually depends on, and the fake-clock tests above can't
// show it. It needs a disposable PostgreSQL database, the same one the
// store's tests use:
//
//	FORGESYNC_TEST_DATABASE_URL=postgres://... go test ./internal/leader/
func openLeaseStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("FORGESYNC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FORGESYNC_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Whoever ran last may still hold the lease; let it go.
	if l, err := s.Leadership(ctx); err == nil && l.Holder != "" {
		if err := s.ReleaseLease(ctx, l.Holder); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// killed is a controller's view of the database when its process dies:
// everything works until the moment it stops, and the lease is never
// given back, because nothing ran to give it back.
type killed struct{ *store.Store }

func (killed) ReleaseLease(context.Context, string) error { return nil }

// The standby takes over when the leader stops answering, and only after
// the lease has run out -- never while the old one might still be acting.
func TestFailoverOverARealDatabase(t *testing.T) {
	db := openLeaseStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ttl = 2 * time.Second
	opts := func(holder, name string) Options {
		return Options{Holder: holder, Name: name, URL: "http://" + name + ":8090", TTL: ttl, Renew: ttl / 3}
	}
	a := New(killed{db}, opts("aaa", "forgesync-a"), slog.New(slog.DiscardHandler))
	b := New(db, opts("bbb", "forgesync-b"), slog.New(slog.DiscardHandler))

	actx, stopA := context.WithCancel(ctx)
	bctx, stopB := context.WithCancel(ctx)
	defer stopB()
	var wg sync.WaitGroup
	wg.Add(2)
	// A starts first, so it takes the lease; B finds it held.
	go func() { defer wg.Done(); a.Run(actx) }()
	waitFor(t, ctx, a, true, "the first controller didn't take the lease")
	go func() { defer wg.Done(); b.Run(bctx) }()

	// Both are running: exactly one of them leads.
	time.Sleep(ttl)
	if a.Leading() == b.Leading() {
		t.Fatalf("both controllers think they lead the same way: a=%v b=%v", a.Leading(), b.Leading())
	}
	if !a.Leading() {
		t.Fatal("the standby took the lease from a leader that was still renewing it")
	}
	if st := b.State(); st.Name != "forgesync-a" {
		t.Errorf("the standby names %q as the leader", st.Name)
	}

	// A goes away without giving the lease up, as a killed process does
	// (killed above swallows the release, which a dead process never made).
	// B waits for the lease to run out before taking over.
	killed := time.Now()
	stopA()
	waitFor(t, ctx, b, true, "the standby never took over")
	if took := time.Since(killed); took < ttl {
		t.Errorf("the standby took over after %s, before the %s lease had run out", took, ttl)
	}
	if a.Leading() {
		t.Error("the old leader still thinks it leads")
	}
	stopB()
	wg.Wait()
}

// A planned stop gives the lease up, so the other one takes over in about
// one renewal instead of waiting for the lease to run out.
func TestAPlannedStopHandsOverQuickly(t *testing.T) {
	db := openLeaseStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ttl = 4 * time.Second
	a := New(db, Options{Holder: "aaa", Name: "forgesync-a", TTL: ttl, Renew: 300 * time.Millisecond},
		slog.New(slog.DiscardHandler))
	b := New(db, Options{Holder: "bbb", Name: "forgesync-b", TTL: ttl, Renew: 300 * time.Millisecond},
		slog.New(slog.DiscardHandler))

	actx, stopA := context.WithCancel(ctx)
	bctx, stopB := context.WithCancel(ctx)
	defer stopB()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(actx) }()
	waitFor(t, ctx, a, true, "the first controller didn't take the lease")
	go func() { defer wg.Done(); b.Run(bctx) }()
	time.Sleep(500 * time.Millisecond)

	stopped := time.Now()
	stopA()
	waitFor(t, ctx, b, true, "the standby never took over after a planned stop")
	if took := time.Since(stopped); took >= ttl {
		t.Errorf("a planned stop took %s to hand over, no better than the %s lease", took, ttl)
	}
	stopB()
	wg.Wait()
}

// Everything that changes anything runs under Supervise, so it has to
// stop when leadership does -- here, when the lease is taken elsewhere.
func TestSupervisedWorkStopsWhenTheLeaseIsLost(t *testing.T) {
	db := openLeaseStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const ttl = 2 * time.Second
	a := New(db, Options{Holder: "aaa", Name: "forgesync-a", TTL: ttl, Renew: ttl / 3}, slog.New(slog.DiscardHandler))
	actx, stopA := context.WithCancel(ctx)
	defer stopA()

	working := make(chan struct{})
	stopped := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(actx) }()
	go func() {
		defer wg.Done()
		a.Supervise(actx, func(lctx context.Context) {
			once.Do(func() { close(working) })
			<-lctx.Done() // the work runs until leadership goes
			close(stopped)
		})
	}()
	select {
	case <-working:
	case <-ctx.Done():
		t.Fatal("the work never started")
	}

	// Another controller takes the lease by force, as one would after this
	// one lost the database and its lease ran out.
	if _, err := db.ReleaseLease(context.Background(), "aaa"), error(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireLease(context.Background(), "bbb", "forgesync-b", "http://b:8091", time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("the work kept running after leadership went")
	}
	if a.Leading() {
		t.Error("the controller still thinks it leads")
	}
	stopA()
	wg.Wait()
}

// waitFor waits until the controller is (or isn't) leading.
func waitFor(t *testing.T, ctx context.Context, e *Elector, leading bool, msg string) {
	t.Helper()
	for {
		if e.Leading() == leading {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(msg)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
