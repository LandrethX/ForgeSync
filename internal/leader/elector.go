// Package leader keeps one ForgeSync controller acting at a time.
//
// The controllers share a PostgreSQL database, so the database is the
// witness: leadership is a lease row taken and renewed there, and a
// controller acts only while it holds one that hasn't run out. A leader
// that can't renew steps down before its lease expires, so the next one to
// take over never overlaps with it -- what fences the old leader off is the
// lease running out, not it noticing.
//
// Everything that changes a Forgejo node or the database's own state runs
// under Supervise, whose context is cancelled the moment leadership is
// lost. The work is all idempotent and picked up again by the next leader,
// so cancelling a scan or a replication run halfway loses nothing.
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// NewHolderID identifies one running controller. It's new on every start,
// so a controller that comes back never inherits its own old lease.
func NewHolderID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// Store is the lease in the database.
type Store interface {
	AcquireLease(ctx context.Context, holder, name, url string, ttl time.Duration) (store.Lease, error)
	Leadership(ctx context.Context) (store.Lease, error)
	ReleaseLease(ctx context.Context, holder string) error
}

// Options are the lease's timings.
type Options struct {
	// Holder is this process's identity: it changes on every restart, so a
	// controller that comes back doesn't inherit its own old lease.
	Holder string
	// Name and URL say who holds it, for people and for the admin UI.
	Name string
	URL  string
	// TTL is how long a lease lasts without a renewal: the longest a
	// failover takes. Renew is how often the leader renews it.
	TTL   time.Duration
	Renew time.Duration
	// Yield, if set, is asked on every renewal while this controller is
	// leading whether it should hand the lease over: a controller meant to
	// lead is back, and this one only took it because that one was away.
	// It gives the lease up and waits a lease's length before taking part
	// again, which is long enough for the other to take it.
	//
	// Leadership itself doesn't depend on this -- a controller that never
	// yields is still correct, just not the preferred one -- so a failure
	// to work out the answer simply means not yielding.
	Yield func(ctx context.Context) bool
}

// State is what this controller can say about leadership.
type State struct {
	// Leading is true only while this controller's own lease is in force.
	Leading bool `json:"leading"`
	// Name, URL and Since describe whoever holds it, this one or another.
	Name  string    `json:"name,omitempty"`
	URL   string    `json:"url,omitempty"`
	Since time.Time `json:"since,omitzero"`
	// Error is the last failure to reach the database, if leadership is
	// unknown because of it.
	Error string `json:"error,omitempty"`
}

// Elector runs one controller's side of the election.
type Elector struct {
	store Store
	opts  Options
	log   *slog.Logger
	now   func() time.Time // monotonic, for judging our own lease

	mu sync.RWMutex
	// until is when our lease runs out, on this machine's clock. Leadership
	// is this and nothing else: if we can't renew, it passes and we stop.
	until time.Time
	// quiet is when this controller may take part again after handing the
	// lease over, so the one it yielded to has time to take it.
	quiet time.Time
	state State
	subs  map[chan struct{}]struct{}
}

func New(s Store, opts Options, log *slog.Logger) *Elector {
	if opts.TTL <= 0 {
		opts.TTL = 15 * time.Second
	}
	if opts.Renew <= 0 || opts.Renew >= opts.TTL {
		opts.Renew = opts.TTL / 3
	}
	return &Elector{store: s, opts: opts, log: log, now: time.Now, subs: map[chan struct{}]struct{}{}}
}

// Leading reports that this controller holds a lease that hasn't run out.
func (e *Elector) Leading() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.now().Before(e.until)
}

// State is the leadership as this controller last saw it.
func (e *Elector) State() State {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := e.state
	s.Leading = e.now().Before(e.until)
	return s
}

// Run takes part in the election until ctx ends, then gives up the lease if
// this controller was holding it.
func (e *Elector) Run(ctx context.Context) {
	t := time.NewTicker(e.opts.Renew)
	defer t.Stop()
	for {
		e.once(ctx)
		select {
		case <-ctx.Done():
			e.stepDown()
			return
		case <-t.C:
		}
	}
}

func (e *Elector) once(ctx context.Context) {
	e.mu.RLock()
	quiet := e.quiet
	e.mu.RUnlock()
	if e.now().Before(quiet) {
		return // just handed over; let the other one take it
	}
	// The call itself must not outlive the lease: a renewal that answers
	// after the lease has run out tells us nothing.
	cctx, cancel := context.WithTimeout(ctx, e.opts.TTL)
	defer cancel()
	sent := e.now()
	lease, err := e.store.AcquireLease(cctx, e.opts.Holder, e.opts.Name, e.opts.URL, e.opts.TTL)
	if err != nil {
		e.mu.Lock()
		e.state.Error = err.Error()
		e.mu.Unlock()
		e.log.Warn("leadership: the database couldn't be reached", "error", err)
		e.notify()
		return
	}
	was := e.Leading()
	e.mu.Lock()
	mine := lease.Holder == e.opts.Holder && lease.Held()
	if mine {
		// Count the lease from when the request went out, not from when the
		// answer came back: the database's clock decides how long it lasts,
		// and the journey back has already used some of it.
		e.until = sent.Add(lease.For())
	} else {
		e.until = time.Time{}
	}
	e.state = State{Name: lease.Name, URL: lease.URL, Since: lease.AcquiredAt}
	if !lease.Held() {
		e.state = State{}
	}
	e.mu.Unlock()
	switch now := e.Leading(); {
	case now && !was:
		e.log.Info("leadership taken", "controller", e.opts.Name, "for", lease.For().Round(time.Second))
	case !now && was:
		e.log.Warn("leadership lost", "controller", e.opts.Name, "holder", lease.Name)
	}
	e.notify()
	if e.Leading() && e.opts.Yield != nil && e.opts.Yield(ctx) {
		e.handOver(ctx)
	}
}

// handOver gives the lease up for a controller that should have it, and
// keeps this one out of the election long enough for that one to take it
// -- two of its renewals, not a whole lease. If it doesn't (it stopped
// between saying it was there and being handed the work), this one takes
// the lease straight back, so the wait costs seconds rather than a lease.
func (e *Elector) handOver(ctx context.Context) {
	e.log.Info("leadership handed over: a controller that should lead is back", "controller", e.opts.Name)
	quiet := 2 * e.opts.Renew
	if quiet > e.opts.TTL {
		quiet = e.opts.TTL
	}
	e.mu.Lock()
	e.until = time.Time{}
	e.quiet = e.now().Add(quiet)
	e.mu.Unlock()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := e.store.ReleaseLease(rctx, e.opts.Holder); err != nil {
		e.log.Warn("leadership: giving the lease up failed; it runs out on its own", "error", err)
	}
	e.notify()
}

// stepDown gives up the lease on shutdown so the other controller takes
// over at once. A failure here only means the wait is the lease's length.
func (e *Elector) stepDown() {
	if !e.Leading() {
		return
	}
	e.mu.Lock()
	e.until = time.Time{}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.ReleaseLease(ctx, e.opts.Holder); err != nil {
		e.log.Warn("leadership: giving up the lease failed", "error", err)
	} else {
		e.log.Info("leadership given up")
	}
	e.notify()
}

// Supervise runs work while this controller is leading and stops it when it
// isn't: work's context is cancelled the moment the lease is lost. It
// returns when ctx ends, after the work has stopped.
func (e *Elector) Supervise(ctx context.Context, work func(context.Context)) {
	for {
		if err := e.wait(ctx, true); err != nil {
			return
		}
		wctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			work(wctx)
		}()
		err := e.wait(ctx, false) // until leadership is lost, or ctx ends
		cancel()
		<-done
		if err != nil {
			return
		}
	}
}

// wait blocks until Leading() is leading, ctx ends (error), or, while
// leading, the lease runs out without a renewal.
func (e *Elector) wait(ctx context.Context, leading bool) error {
	ch, unsubscribe := e.subscribe()
	defer unsubscribe()
	for {
		if e.Leading() == leading {
			return nil
		}
		// A lease can run out without anything happening, so don't only
		// wait to be told.
		t := time.NewTimer(e.opts.Renew)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

func (e *Elector) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

func (e *Elector) notify() {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for ch := range e.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
