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
	// RecordUplink notes that this controller lost or regained its own
	// reach, which is a different thing from a node being down and worth
	// one line rather than one per node.
	RecordUplink(ctx context.Context, up bool, detail string) error
}

// Target is one node to watch.
type Target struct {
	Name        string
	ServiceUser string
	Client      Checker
}

// Options are how often the nodes are contacted, how long each contact
// may take, and how many failures in a row make a node unreachable.
type Options struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
	// Uplink is asked, when every node has gone quiet at once, whether
	// this controller can reach anything at all. nil turns the question
	// off and every node is recorded as unreachable as before.
	Uplink Uplink
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
	// uplink is what the last probe said and when, so five nodes failing
	// together ask once rather than five times.
	uplinkUp    bool
	uplinkAsked time.Time
	uplinkKnown bool

	subsMu sync.Mutex
	subs   map[chan struct{}]struct{}
}

// NewMonitor watches targets and reports every change to rec.
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

	// Five nodes in five countries do not usually go together. When they
	// do, and this controller cannot reach anything else either, the
	// fault is at this end, and recording it as five nodes failing would
	// put the wrong thing in the history and leave the right one unsaid.
	if s.State != Healthy && m.opts.Uplink != nil && m.allQuiet() && !m.uplinkIsUp(ctx) {
		return s
	}

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

// allQuiet reports that no node is currently answering. A node that has
// not been checked yet does not count as quiet: a controller still
// starting up has not learned anything to be suspicious about.
func (m *Monitor) allQuiet() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.status) < len(m.targets) {
		return false
	}
	for _, s := range m.status {
		if s.State == Healthy {
			return false
		}
	}
	return len(m.status) > 0
}

// uplinkIsUp asks whether this controller can reach anything, at most
// once per interval however many nodes ask, and records the answer when
// it changes.
func (m *Monitor) uplinkIsUp(ctx context.Context) bool {
	m.mu.Lock()
	fresh := m.uplinkKnown && m.now().Sub(m.uplinkAsked) < m.opts.Interval
	if fresh {
		up := m.uplinkUp
		m.mu.Unlock()
		return up
	}
	m.mu.Unlock()

	up := m.opts.Uplink.Up(ctx)

	m.mu.Lock()
	was, known := m.uplinkUp, m.uplinkKnown
	m.uplinkUp, m.uplinkAsked, m.uplinkKnown = up, m.now(), true
	m.mu.Unlock()

	if !known || was != up {
		if up {
			m.log.Info("this controller can reach the network again")
		} else {
			m.log.Warn("this controller cannot reach the network: the nodes are not being recorded as down, because the fault is at this end")
		}
		if m.rec != nil && ctx.Err() == nil {
			detail := "no reference answered"
			if up {
				detail = "a reference answered again"
			}
			if err := m.rec.RecordUplink(ctx, up, detail); err != nil {
				m.log.Error("recording the uplink failed", "error", err)
			}
		}
	}
	return up
}

// UplinkUp reports what the last probe said. It is true when the check is
// off or has never had reason to run, because then nothing suggests
// otherwise.
func (m *Monitor) UplinkUp() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.uplinkKnown || m.uplinkUp
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
