package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// syncBuffer is a bytes.Buffer safe to read while the follower writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// liveHistory is a controller whose history grows during a test. It honours
// category, from, limit and cursor like the real one.
type liveHistory struct {
	mu        sync.Mutex
	events    []store.Event
	failNext  int // answer this many requests with 500
	forbidden bool
	queries   []string
}

func (h *liveHistory) add(e store.Event) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
}

func (h *liveHistory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queries = append(h.queries, r.URL.RawQuery)
	if h.forbidden {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"this needs the operator role"}`))
		return
	}
	if h.failNext > 0 {
		h.failNext--
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"database unavailable"}`))
		return
	}
	q := r.URL.Query()
	var from time.Time
	if v := q.Get("from"); v != "" {
		from, _ = time.Parse(time.RFC3339, v)
	}
	var match []store.Event
	for _, e := range h.events {
		if (q.Get("category") == "" || strings.Contains(","+q.Get("category")+",", ","+e.Category+",")) && !e.At.Before(from) {
			match = append(match, e)
		}
	}
	sort.Slice(match, func(i, j int) bool { return match[i].At.After(match[j].At) })
	start := 0
	fmt.Sscan(q.Get("cursor"), &start)
	limit := 100
	fmt.Sscan(q.Get("limit"), &limit)
	end := min(start+limit, len(match))
	res := map[string]any{"items": match[min(start, end):end]}
	if end < len(match) {
		res["next_cursor"] = fmt.Sprint(end)
	}
	json.NewEncoder(w).Encode(res)
}

var fbase = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

func ev(n int, at time.Time) store.Event {
	return store.Event{ID: fmt.Sprintf("a%d", n), At: at, Category: "repo", Actor: "sceneid:alice",
		Action: "repo.set_primary", Target: fmt.Sprintf("alice/r%d", n), Details: map[string]any{}}
}

func startFollow(t *testing.T, h *liveHistory, jsonOut bool, params map[string]string) (*syncBuffer, *syncBuffer, func() error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	out, errOut := &syncBuffer{}, &syncBuffer{}
	p := map[string][]string{}
	for k, v := range params {
		p[k] = []string{v}
	}
	fw := &follower{
		o:      &options{server: srv.URL, token: "tok"},
		params: p, jsonOut: jsonOut, out: out, err: errOut, interval: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fw.run(ctx, 10) }()
	return out, errOut, func() error {
		cancel()
		return <-done
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// targets lists the targets printed, in order.
func targets(out string) []string {
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		f := strings.Fields(line)
		got = append(got, f[len(f)-1])
	}
	return got
}

func TestFollowPrintsNewEntriesOnce(t *testing.T) {
	h := &liveHistory{}
	for i := 1; i <= 15; i++ {
		h.add(ev(i, fbase.Add(time.Duration(i)*time.Second)))
	}
	out, _, stop := startFollow(t, h, false, map[string]string{"category": "repo"})

	waitFor(t, "the first screen", func() bool { return strings.Count(out.String(), "\n") >= 11 })
	first := targets(out.String())
	if len(first) != 10 || first[0] != "alice/r6" || first[9] != "alice/r15" {
		t.Fatalf("first screen should be the newest 10, oldest first: %v", first)
	}
	// Let a few polls run: r1..r5 are inside the look-back window but must not appear.
	time.Sleep(60 * time.Millisecond)

	h.add(ev(16, fbase.Add(20*time.Second)))
	// Written late with an earlier time (like a node change recorded after its check).
	h.add(ev(17, fbase.Add(15*time.Second+500*time.Millisecond)))
	waitFor(t, "the new entries", func() bool { return strings.Contains(out.String(), "alice/r16") })
	time.Sleep(60 * time.Millisecond) // more polls: nothing may repeat

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	got := targets(out.String())
	want := []string{"alice/r6", "alice/r7", "alice/r8", "alice/r9", "alice/r10", "alice/r11", "alice/r12",
		"alice/r13", "alice/r14", "alice/r15", "alice/r17", "alice/r16"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("printed %v\nwant    %v", got, want)
	}
	h.mu.Lock()
	for _, q := range h.queries {
		if !strings.Contains(q, "category=repo") {
			t.Errorf("a poll lost the filter: %s", q)
		}
	}
	h.mu.Unlock()
}

func TestFollowJSONLines(t *testing.T) {
	h := &liveHistory{}
	h.add(ev(1, fbase))
	out, _, stop := startFollow(t, h, true, nil)
	waitFor(t, "first entry", func() bool { return strings.Contains(out.String(), "alice/r1") })
	h.add(ev(2, fbase.Add(time.Second)))
	waitFor(t, "second entry", func() bool { return strings.Contains(out.String(), "alice/r2") })
	stop()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, l := range lines {
		var e store.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Errorf("not one JSON object per line: %q", l)
		}
	}
	if len(lines) != 2 {
		t.Errorf("%d lines, want 2 (and no header)", len(lines))
	}
}

func TestFollowRetriesThenStopsOnForbidden(t *testing.T) {
	h := &liveHistory{}
	h.add(ev(1, fbase))
	out, errOut, stop := startFollow(t, h, false, nil)
	waitFor(t, "first entry", func() bool { return strings.Contains(out.String(), "alice/r1") })

	h.mu.Lock()
	h.failNext = 2
	h.mu.Unlock()
	waitFor(t, "recovery", func() bool { return strings.Contains(errOut.String(), "connection restored") })
	if !strings.Contains(errOut.String(), "database unavailable (retrying)") {
		t.Errorf("stderr = %q", errOut.String())
	}
	h.add(ev(2, fbase.Add(time.Second)))
	waitFor(t, "entry after recovery", func() bool { return strings.Contains(out.String(), "alice/r2") })

	h.mu.Lock()
	h.forbidden = true
	h.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if err := stop(); err == nil || !strings.Contains(err.Error(), "needs the operator role") {
		t.Errorf("err = %v, want the 403 to end following", err)
	}
}

func TestFollowFlags(t *testing.T) {
	srv := httptest.NewServer(&liveHistory{})
	defer srv.Close()
	for _, args := range [][]string{
		{"history", "-f", "--export", "csv"},
		{"history", "-f", "--to", "2026-09-30"},
		{"history", "-f", "--interval", "100ms"},
	} {
		if _, _, err := run(t, srv, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
