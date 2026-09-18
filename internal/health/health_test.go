package health

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
)

var t0 = time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)

func TestNextStateMachine(t *testing.T) {
	ok := Result{Reachable: true, ForgejoOK: true, AuthOK: true, Version: "16.0.5"}
	down := Result{Err: errors.New("connection refused")}

	s := Status{Node: "dk", State: Unknown}
	s = Next(s, ok, 3, t0)
	if s.State != Healthy || s.LastSeen == nil || !s.LastSeen.Equal(t0) || s.FailingSince != nil || s.Version != "16.0.5" {
		t.Fatalf("after ok: %+v", s)
	}

	// A blip: SUSPECT, not UNREACHABLE; last seen stays at the last success.
	s = Next(s, down, 3, t0.Add(15*time.Second))
	if s.State != Suspect || s.ConsecutiveFailures != 1 || !s.LastSeen.Equal(t0) || !s.FailingSince.Equal(t0.Add(15*time.Second)) {
		t.Fatalf("after 1 failure: %+v", s)
	}
	s = Next(s, down, 3, t0.Add(30*time.Second))
	if s.State != Suspect {
		t.Fatalf("after 2 failures: %+v", s)
	}
	s = Next(s, down, 3, t0.Add(45*time.Second))
	if s.State != Unreachable || s.ConsecutiveFailures != 3 || s.LastError != "connection refused" {
		t.Fatalf("after 3 failures: %+v", s)
	}
	// FailingSince keeps the start of the outage.
	if !s.FailingSince.Equal(t0.Add(15 * time.Second)) {
		t.Fatalf("failing since moved: %+v", s)
	}

	s = Next(s, ok, 3, t0.Add(60*time.Second))
	if s.State != Healthy || s.ConsecutiveFailures != 0 || s.FailingSince != nil || s.LastError != "" {
		t.Fatalf("after recovery: %+v", s)
	}
}

func TestNextReachableProblems(t *testing.T) {
	s := Next(Status{State: Healthy}, Result{Reachable: true, ForgejoOK: true, AuthOK: false, Err: errors.New("401")}, 3, t0)
	if s.State != AuthError || s.LastSeen == nil || s.FailingSince == nil {
		t.Errorf("auth failure: %+v", s)
	}
	s = Next(Status{State: Healthy}, Result{Reachable: true, ForgejoOK: false, AuthOK: true}, 3, t0)
	if s.State != Degraded {
		t.Errorf("healthz fail: %+v", s)
	}
}

// fakeForgejo serves the three endpoints the monitor calls.
type fakeForgejo struct {
	healthz  int // HTTP status for /api/healthz
	userCode int // HTTP status for /api/v1/user
	login    string
	admin    bool
}

func (f fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/healthz":
		w.WriteHeader(f.healthz)
		status := "pass"
		if f.healthz != 200 {
			status = "fail"
		}
		io.WriteString(w, `{"status":"`+status+`"}`)
	case "/api/v1/version":
		io.WriteString(w, `{"version":"16.0.5"}`)
	case "/api/v1/user":
		w.WriteHeader(f.userCode)
		if f.userCode == 200 {
			admin := "false"
			if f.admin {
				admin = "true"
			}
			io.WriteString(w, `{"login":"`+f.login+`","is_admin":`+admin+`}`)
		} else {
			io.WriteString(w, `{"message":"invalid token"}`)
		}
	}
}

type fakeRecorder struct {
	mu          sync.Mutex
	transitions []string
}

func (r *fakeRecorder) RecordNodeStatus(_ context.Context, s Status, prev State) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.State != prev {
		r.transitions = append(r.transitions, string(prev)+"->"+string(s.State))
	}
	return nil
}

func TestMonitorChecks(t *testing.T) {
	cases := map[string]struct {
		forgejo fakeForgejo
		want    State
	}{
		"healthy":           {fakeForgejo{healthz: 200, userCode: 200, login: "forgesync", admin: true}, Healthy},
		"token rejected":    {fakeForgejo{healthz: 200, userCode: 401}, AuthError},
		"wrong account":     {fakeForgejo{healthz: 200, userCode: 200, login: "alice", admin: true}, AuthError},
		"not a site admin":  {fakeForgejo{healthz: 200, userCode: 200, login: "forgesync", admin: false}, AuthError},
		"forgejo unhealthy": {fakeForgejo{healthz: 500, userCode: 200, login: "forgesync", admin: true}, Degraded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.forgejo)
			defer srv.Close()
			c, _ := forgejo.New(srv.URL, "tok", nil)
			m := NewMonitor([]Target{{Name: "se", ServiceUser: "forgesync", Client: c}},
				Options{Interval: time.Hour, Timeout: 2 * time.Second, FailureThreshold: 3}, nil, slog.New(slog.DiscardHandler))
			s := m.CheckOnce(context.Background(), m.targets[0])
			if s.State != tc.want {
				t.Fatalf("state = %s, want %s (error: %s)", s.State, tc.want, s.LastError)
			}
		})
	}
}

func TestMonitorRunDetectsOutageAndRecovery(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	healthy := fakeForgejo{healthz: 200, userCode: 200, login: "forgesync", admin: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			// Simulate a dead node: drop the connection without a response.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		healthy.ServeHTTP(w, r)
	}))
	defer srv.Close()

	c, _ := forgejo.New(srv.URL, "tok", nil)
	rec := &fakeRecorder{}
	m := NewMonitor([]Target{{Name: "dk", ServiceUser: "forgesync", Client: c}},
		Options{Interval: 20 * time.Millisecond, Timeout: 10 * time.Millisecond, FailureThreshold: 2}, rec, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitFor := func(want State) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if m.Snapshot()[0].State == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("node never reached %s; last status %+v", want, m.Snapshot()[0])
	}
	waitFor(Healthy)
	up.Store(false)
	waitFor(Unreachable)
	up.Store(true)
	waitFor(Healthy)
	cancel()
	<-done

	rec.mu.Lock()
	defer rec.mu.Unlock()
	want := []string{"UNKNOWN->HEALTHY", "HEALTHY->SUSPECT", "SUSPECT->UNREACHABLE", "UNREACHABLE->HEALTHY"}
	if len(rec.transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", rec.transitions, want)
	}
	for i := range want {
		if rec.transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", rec.transitions, want)
		}
	}
}
