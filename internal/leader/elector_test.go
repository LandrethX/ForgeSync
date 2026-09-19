package leader

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// fakeLease is the one lease row, shared by the controllers in a test.
type fakeLease struct {
	mu     sync.Mutex
	now    time.Time
	lease  store.Lease
	err    error
	grants int
}

func (f *fakeLease) tick(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeLease) AcquireLease(_ context.Context, holder, name, url string, ttl time.Duration) (store.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Lease{}, f.err
	}
	held := f.lease.Holder != "" && f.lease.ExpiresAt.After(f.now)
	if !held || f.lease.Holder == holder {
		acquired := f.now
		if held && f.lease.Holder == holder {
			acquired = f.lease.AcquiredAt
		} else {
			f.grants++
		}
		f.lease = store.Lease{Holder: holder, Name: name, URL: url, AcquiredAt: acquired,
			RenewedAt: f.now, ExpiresAt: f.now.Add(ttl)}
	}
	l := f.lease
	l.Now = f.now
	return l, nil
}

func (f *fakeLease) Leadership(context.Context) (store.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.lease
	l.Now = f.now
	return l, f.err
}

func (f *fakeLease) ReleaseLease(_ context.Context, holder string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease.Holder == holder {
		f.lease.ExpiresAt = f.now
	}
	return nil
}

func newElector(f *fakeLease, holder, name string, clock *time.Time) *Elector {
	e := New(f, Options{Holder: holder, Name: name, TTL: 15 * time.Second, Renew: 5 * time.Second},
		slog.New(slog.DiscardHandler))
	e.now = func() time.Time { return *clock }
	return e
}

func TestOnlyOneLeadsAtATime(t *testing.T) {
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeLease{now: start}
	clock := start
	a := newElector(f, "aaa", "forgesync-a", &clock)
	b := newElector(f, "bbb", "forgesync-b", &clock)
	ctx := context.Background()

	a.once(ctx)
	b.once(ctx)
	if !a.Leading() || b.Leading() {
		t.Fatalf("a %v b %v: a took it first", a.Leading(), b.Leading())
	}
	if st := b.State(); st.Name != "forgesync-a" || st.Leading {
		t.Errorf("b's view = %+v", st)
	}

	// While a renews, b never gets in.
	for i := 0; i < 5; i++ {
		f.tick(5 * time.Second)
		clock = clock.Add(5 * time.Second)
		a.once(ctx)
		b.once(ctx)
		if !a.Leading() || b.Leading() {
			t.Fatalf("round %d: a %v b %v", i, a.Leading(), b.Leading())
		}
	}
	if f.grants != 1 {
		t.Errorf("leadership changed hands %d times", f.grants-1)
	}
}

func TestTheLeaseRunsOutOnItsOwn(t *testing.T) {
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeLease{now: start}
	clock := start
	a := newElector(f, "aaa", "forgesync-a", &clock)
	a.once(context.Background())
	if !a.Leading() {
		t.Fatal("a should lead")
	}
	// The database becomes unreachable: a keeps going until its lease runs
	// out on its own clock, and then stops without being told.
	f.mu.Lock()
	f.err = errors.New("connection refused")
	f.mu.Unlock()
	clock = clock.Add(14 * time.Second)
	a.once(context.Background())
	if !a.Leading() {
		t.Error("a stopped before its lease had run out")
	}
	clock = clock.Add(2 * time.Second)
	if a.Leading() {
		t.Error("a is still acting after its lease ran out")
	}
	if st := a.State(); st.Error == "" {
		t.Errorf("the reason isn't shown: %+v", st)
	}
}

func TestTheStandbyTakesOverWhenTheLeaderStops(t *testing.T) {
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeLease{now: start}
	clock := start
	a := newElector(f, "aaa", "forgesync-a", &clock)
	b := newElector(f, "bbb", "forgesync-b", &clock)
	ctx := context.Background()
	a.once(ctx)
	b.once(ctx)

	// a goes away without a word; b waits out the lease and takes over.
	f.tick(16 * time.Second)
	clock = clock.Add(16 * time.Second)
	b.once(ctx)
	if !b.Leading() {
		t.Fatal("b didn't take over")
	}
	if st := b.State(); st.Name != "forgesync-b" || !st.Leading {
		t.Errorf("b's view = %+v", st)
	}
	// a comes back and finds it has been replaced.
	a.once(ctx)
	if a.Leading() {
		t.Error("a took the lease back while b holds it")
	}
}

func TestSupervisorRunsWorkOnlyWhileLeading(t *testing.T) {
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeLease{now: start}
	e := New(f, Options{Holder: "aaa", Name: "forgesync-a", TTL: time.Second, Renew: 20 * time.Millisecond},
		slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	running := make(chan struct{}, 4)
	stopped := make(chan struct{}, 4)
	go e.Supervise(ctx, func(wctx context.Context) {
		running <- struct{}{}
		<-wctx.Done()
		stopped <- struct{}{}
	})
	select {
	case <-running:
	case <-time.After(2 * time.Second):
		t.Fatal("the work never started")
	}

	// Another controller takes the lease: the work stops.
	f.mu.Lock()
	f.lease = store.Lease{Holder: "bbb", Name: "forgesync-b", AcquiredAt: f.now,
		RenewedAt: f.now, ExpiresAt: f.now.Add(time.Hour)}
	f.mu.Unlock()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the work kept going after the lease was lost")
	}
}

// A controller that took the lease while the preferred one was away hands
// it back when that one returns: someone has to decide which of two
// healthy controllers acts, and "whoever started first" isn't a decision
// anyone made.
func TestTheStandbyHandsBackToThePreferredController(t *testing.T) {
	f := &fakeLease{now: time.Now()}
	clock := f.now
	yield := true
	b := newElector(f, "bbb", "forgesync-b", &clock)
	b.opts.Yield = func(context.Context) bool { return yield }

	// b is alone, so it leads.
	yield = false
	b.once(context.Background())
	if !b.Leading() {
		t.Fatal("the only controller didn't take the lease")
	}

	// a comes back: b gives the lease up and stays out of the election
	// for a lease's length, so a can take it rather than b taking it back.
	yield = true
	b.once(context.Background())
	if b.Leading() {
		t.Fatal("it kept the lease")
	}
	b.once(context.Background())
	if b.Leading() {
		t.Error("it took the lease straight back instead of waiting")
	}

	// a takes it during that window, which is the point of the wait: the
	// lease was given up, not left to run out.
	a := newElector(f, "aaa", "forgesync-a", &clock)
	a.once(context.Background())
	if !a.Leading() {
		t.Fatal("the preferred controller couldn't take the lease")
	}

	// Once the window has passed, b takes part again but finds it held.
	clock = clock.Add(20 * time.Second)
	f.tick(20 * time.Second)
	yield = false
	a.once(context.Background()) // a renews
	b.once(context.Background())
	if b.Leading() {
		t.Error("b took the lease from a controller that was renewing it")
	}
	if !a.Leading() {
		t.Error("a lost the lease it was renewing")
	}
}
