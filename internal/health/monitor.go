package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
)

// Checker is the part of the Forgejo client the monitor needs.
type Checker interface {
	Healthz(ctx context.Context) (forgejo.Healthz, error)
	Version(ctx context.Context) (string, error)
	CurrentUser(ctx context.Context) (forgejo.User, error)
}

// Recorder persists status changes. prev is the state before this check.
type Recorder interface {
	RecordNodeStatus(ctx context.Context, s Status, prev State) error
}

// Target is one node to watch.
type Target struct {
	Name        string
	ServiceUser string
	Client      Checker
}

type Options struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
}

// Monitor checks every node on an interval and keeps the latest status in memory.
type Monitor struct {
	targets []Target
	opts    Options
	rec     Recorder
	log     *slog.Logger
	now     func() time.Time

	mu     sync.RWMutex
	status map[string]Status

	subsMu sync.Mutex
	subs   map[chan struct{}]struct{}
}

func NewMonitor(targets []Target, opts Options, rec Recorder, log *slog.Logger) *Monitor {
	m := &Monitor{
		targets: targets,
		opts:    opts,
		rec:     rec,
		log:     log,
		now:     time.Now,
		status:  make(map[string]Status, len(targets)),
		subs:    map[chan struct{}]struct{}{},
	}
	for _, t := range targets {
		m.status[t.Name] = Status{Node: t.Name, State: Unknown}
	}
	return m
}

// Restore sets the state each node was last known to be in, so a
// controller that has just started doesn't record every node changing from
// UNKNOWN. Anything it doesn't name stays UNKNOWN. Call it before Run.
func (m *Monitor) Restore(states map[string]State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, state := range states {
		s, ok := m.status[name]
		if !ok || !state.Valid() {
			continue
		}
		s.State = state
		m.status[name] = s
	}
}

// Run checks every node immediately and then on each interval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range m.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.watch(ctx, t)
		}()
	}
	wg.Wait()
}

func (m *Monitor) watch(ctx context.Context, t Target) {
	ticker := time.NewTicker(m.opts.Interval)
	defer ticker.Stop()
	for {
		m.CheckOnce(ctx, t)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// CheckOnce runs one round of checks for a node and records the result.
func (m *Monitor) CheckOnce(ctx context.Context, t Target) Status {
	cctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	r := check(cctx, t)
	cancel()

	m.mu.Lock()
	prev := m.status[t.Name]
	s := Next(prev, r, m.opts.FailureThreshold, m.now().UTC())
	m.status[t.Name] = s
	m.mu.Unlock()

	m.notify()
	if s.State != prev.State {
		m.log.Info("node state changed", "node", t.Name, "from", prev.State, "to", s.State, "error", s.LastError)
	}
	if m.rec != nil && ctx.Err() == nil {
		if err := m.rec.RecordNodeStatus(ctx, s, prev.State); err != nil {
			m.log.Error("recording node status failed", "node", t.Name, "error", err)
		}
	}
	return s
}

func check(ctx context.Context, t Target) Result {
	var r Result
	h, err := t.Client.Healthz(ctx)
	var apiErr *forgejo.APIError
	switch {
	case err == nil:
		r.Reachable = true
		r.ForgejoOK = h.Status == "pass" || h.Status == "warn"
		if !r.ForgejoOK {
			r.Err = fmt.Errorf("healthz status %q", h.Status)
		}
	case errors.As(err, &apiErr):
		// Forgejo answered, but not with a healthy response.
		r.Reachable = true
		r.Err = err
	default:
		r.Err = err
		return r
	}

	if v, err := t.Client.Version(ctx); err == nil {
		r.Version = v
	}

	u, err := t.Client.CurrentUser(ctx)
	switch {
	case err != nil:
		if r.Err == nil {
			r.Err = err
		}
		if !forgejo.IsAuthError(err) && !errors.As(err, &apiErr) {
			// The connection broke between the checks.
			r.Reachable = false
		}
	case u.Login != t.ServiceUser:
		r.Err = fmt.Errorf("token belongs to %q, expected %q", u.Login, t.ServiceUser)
	case !u.IsAdmin:
		r.Err = fmt.Errorf("service account %q is not a site admin", u.Login)
	default:
		r.AuthOK = true
	}
	return r
}

// Subscribe returns a channel that receives a value after every check
// (coalesced: a slow reader sees one pending notification, not a backlog).
// Call the returned function to unsubscribe.
func (m *Monitor) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.subsMu.Lock()
	m.subs[ch] = struct{}{}
	m.subsMu.Unlock()
	return ch, func() {
		m.subsMu.Lock()
		delete(m.subs, ch)
		m.subsMu.Unlock()
	}
}

func (m *Monitor) notify() {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Snapshot returns the latest status of every node, sorted by name.
func (m *Monitor) Snapshot() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.status))
	for _, s := range m.status {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}
