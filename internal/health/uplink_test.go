package health

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
)

type fakeUplink struct {
	up    atomic.Bool
	asked atomic.Int32
}

func (f *fakeUplink) Up(context.Context) bool {
	f.asked.Add(1)
	return f.up.Load()
}

// Two dead nodes and a dead uplink is one fact about this controller, not
// two about the installation. Recording it as two node failures puts the
// wrong thing in the history and leaves the right one unsaid.
func TestNodesAreNotBlamedWhenThisControllerIsCutOff(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	c, _ := forgejo.New(dead.URL, "tok", nil)

	link := &fakeUplink{}
	link.up.Store(false)
	rec := &fakeRecorder{}
	m := NewMonitor(
		[]Target{{Name: "se", ServiceUser: "forgesync", Client: c}, {Name: "dk", ServiceUser: "forgesync", Client: c}},
		Options{Interval: time.Hour, Timeout: 200 * time.Millisecond, FailureThreshold: 1, Uplink: link},
		rec, slog.New(slog.DiscardHandler))

	ctx := context.Background()
	m.CheckOnce(ctx, m.targets[0])
	m.CheckOnce(ctx, m.targets[1])

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.transitions) != 0 {
		t.Errorf("the nodes were recorded as failing: %v", rec.transitions)
	}
	if len(rec.uplink) != 1 || rec.uplink[0] {
		t.Errorf("the uplink was recorded as %v, want one entry saying it is down", rec.uplink)
	}
	if m.UplinkUp() {
		t.Error("UplinkUp says the network is fine")
	}
	// The status is still kept, so the pages show what is happening; it
	// is only the history that is spared the wrong story.
	if m.Snapshot()[0].State == Healthy {
		t.Error("the node was left looking healthy")
	}
}

// The first node to notice asks; the rest use that answer, so five nodes
// failing together do not send five rounds of queries.
func TestTheUplinkIsAskedOncePerInterval(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	c, _ := forgejo.New(dead.URL, "tok", nil)

	link := &fakeUplink{}
	link.up.Store(false)
	var targets []Target
	for _, n := range []string{"se", "dk", "de", "uk", "us"} {
		targets = append(targets, Target{Name: n, ServiceUser: "forgesync", Client: c})
	}
	m := NewMonitor(targets, Options{Interval: time.Hour, Timeout: 200 * time.Millisecond,
		FailureThreshold: 1, Uplink: link}, &fakeRecorder{}, slog.New(slog.DiscardHandler))

	for i := range targets {
		m.CheckOnce(context.Background(), m.targets[i])
	}
	if got := link.asked.Load(); got != 1 {
		t.Errorf("the uplink was asked %d times for five nodes, want 1", got)
	}
}

// A node that is genuinely down while the network is fine is still the
// node's problem, and is recorded as always.
func TestANodeThatIsDownWithAWorkingUplinkIsStillRecorded(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer dead.Close()
	c, _ := forgejo.New(dead.URL, "tok", nil)

	link := &fakeUplink{}
	link.up.Store(true)
	rec := &fakeRecorder{}
	m := NewMonitor([]Target{{Name: "se", ServiceUser: "forgesync", Client: c}},
		Options{Interval: time.Hour, Timeout: 200 * time.Millisecond, FailureThreshold: 1, Uplink: link},
		rec, slog.New(slog.DiscardHandler))

	m.CheckOnce(context.Background(), m.targets[0])
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.transitions) == 0 {
		t.Error("a node that is down with a working uplink was not recorded")
	}
	if !m.UplinkUp() {
		t.Error("UplinkUp says the network is gone when the probe said otherwise")
	}
}

// One healthy node means the network is fine, so nothing is asked at all.
func TestNothingIsAskedWhileAnyNodeAnswers(t *testing.T) {
	good := httptest.NewServer(fakeForgejo{healthz: 200, userCode: 200, login: "forgesync", admin: true})
	defer good.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer bad.Close()
	gc, _ := forgejo.New(good.URL, "tok", nil)
	bc, _ := forgejo.New(bad.URL, "tok", nil)

	link := &fakeUplink{}
	link.up.Store(false)
	rec := &fakeRecorder{}
	m := NewMonitor([]Target{{Name: "se", ServiceUser: "forgesync", Client: gc},
		{Name: "dk", ServiceUser: "forgesync", Client: bc}},
		Options{Interval: time.Hour, Timeout: 200 * time.Millisecond, FailureThreshold: 1, Uplink: link},
		rec, slog.New(slog.DiscardHandler))

	m.CheckOnce(context.Background(), m.targets[0])
	m.CheckOnce(context.Background(), m.targets[1])
	if got := link.asked.Load(); got != 0 {
		t.Errorf("the uplink was asked %d times while a node was answering", got)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.transitions) == 0 {
		t.Error("the node that is really down was not recorded")
	}
}

// An empty list is how somebody turns the question off.
func TestNoAddressesMeansNoProber(t *testing.T) {
	if p := NewResolvers(nil, time.Second); p != nil {
		t.Error("an empty list made a prober")
	}
	if p := NewResolvers([]string{"", "  "}, time.Second); p != nil {
		t.Error("a list of nothing made a prober")
	}
	p := NewResolvers([]string{"1.1.1.1", "9.9.9.9:5353"}, time.Second)
	r, ok := p.(*Resolvers)
	if !ok {
		t.Fatal("not a *Resolvers")
	}
	if r.Addresses[0] != "1.1.1.1:53" || r.Addresses[1] != "9.9.9.9:5353" {
		t.Errorf("addresses = %v", r.Addresses)
	}
}

// Nothing listening anywhere is what "no uplink" looks like.
func TestResolversSayDownWhenNothingAnswers(t *testing.T) {
	// 203.0.113.0/24 is reserved for documentation and is not routed.
	p := NewResolvers([]string{"203.0.113.1", "203.0.113.2"}, 700*time.Millisecond)
	if p.Up(context.Background()) {
		t.Error("it said the network was fine with nothing to answer")
	}
}
