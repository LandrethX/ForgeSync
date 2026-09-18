package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"90m": 90 * time.Minute, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "1d12h": 36 * time.Hour,
	} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "7x", "d", "0h", "-1h", "xd"} {
		if _, err := parseSince(bad); err == nil {
			t.Errorf("parseSince(%q) accepted", bad)
		}
	}
}

func TestParseWhen(t *testing.T) {
	start, err := parseWhen("2026-09-18", false)
	if err != nil || !start.Equal(time.Date(2026, 9, 18, 0, 0, 0, 0, time.Local)) {
		t.Errorf("start date = %v, %v", start, err)
	}
	end, err := parseWhen("2026-09-18", true)
	if err != nil || !end.Equal(time.Date(2026, 9, 19, 0, 0, 0, 0, time.Local)) {
		t.Errorf("end date should include the whole day: %v, %v", end, err)
	}
	exact, err := parseWhen("2026-09-18T10:30:00+02:00", true)
	if err != nil || exact.UTC().Hour() != 8 {
		t.Errorf("RFC 3339 = %v, %v", exact, err)
	}
	if _, err := parseWhen("18/09/2026", false); err == nil {
		t.Error("accepted an unknown date format")
	}
}

func TestHistoryParams(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	f := &historyFlags{categories: []string{"node", "repo"}, actor: "sceneid:alice", query: "demo", since: "24h", limit: 50}
	v, err := f.params(now)
	if err != nil {
		t.Fatal(err)
	}
	if v.Get("category") != "node,repo" || v.Get("actor") != "sceneid:alice" || v.Get("q") != "demo" ||
		v.Get("from") != "2026-09-17T12:00:00Z" || v.Get("to") != "" {
		t.Errorf("params = %v", v)
	}
	for name, bad := range map[string]historyFlags{
		"file without export": {file: "x.csv", limit: 1},
		"unknown export":      {export: "xml", limit: 1},
		"zero limit":          {limit: 0},
		"bad from":            {from: "yesterday", limit: 1},
	} {
		if _, err := bad.params(now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// historyServer serves `total` events newest first, in pages, and an export.
func historyServer(t *testing.T, total int, seen *[]string) *httptest.Server {
	t.Helper()
	base := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.RawQuery)
		switch r.URL.Path {
		case "/api/v1/history":
			q := r.URL.Query()
			start := 0
			fmt.Sscan(q.Get("cursor"), &start)
			var limit int
			fmt.Sscan(q.Get("limit"), &limit)
			var items []map[string]any
			for i := start; i < min(start+limit, total); i++ {
				items = append(items, map[string]any{
					"id": fmt.Sprintf("a%d", total-i), "at": base.Add(-time.Duration(i) * time.Minute),
					"category": "repo", "actor": "sceneid:alice", "action": "repo.set_primary",
					"target": "alice/demo", "details": map[string]any{"to": "se", "note": "line1\nline2"},
				})
			}
			res := map[string]any{"items": items}
			if start+limit < total {
				res["next_cursor"] = fmt.Sprint(start + limit)
			}
			json.NewEncoder(w).Encode(res)
		case "/api/v1/history/export":
			if r.URL.Query().Get("format") != "csv" || r.URL.Query().Get("category") != "session" {
				t.Errorf("export query = %s", r.URL.RawQuery)
			}
			w.Write([]byte("time,category,actor,action,target,details\n2026-09-18T10:00:00Z,session,token,session.sign_in,10.0.0.1,{}\n"))
		default:
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"this needs the operator role"}`))
		}
	}))
}

func run(t *testing.T, srv *httptest.Server, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--server", srv.URL, "--token", "tok"}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestHistoryPaging(t *testing.T) {
	var seen []string
	srv := historyServer(t, 7, &seen)
	defer srv.Close()

	out, errOut, err := run(t, srv, "history", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "TIME") {
		t.Fatalf("want header + 5 rows:\n%s", out)
	}
	if !strings.Contains(errOut, "showing the newest 5") {
		t.Errorf("no hint that more exist: %q", errOut)
	}

	seen = nil
	out, errOut, err = run(t, srv, "history", "--limit", "100", "--details", "-c", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n != 8 || errOut != "" {
		t.Errorf("all 7 rows, no hint: %d lines, stderr %q", n, errOut)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "category=repo") || !strings.Contains(seen[0], "limit=100") {
		t.Errorf("requests = %v", seen)
	}
	if strings.Contains(out, "line1\nline2") || !strings.Contains(out, `"to":"se"`) {
		t.Errorf("details should be one line per entry:\n%s", out)
	}

	out, _, err = run(t, srv, "history", "--limit", "3", "-o", "json")
	var events []map[string]any
	if err != nil || json.Unmarshal([]byte(out), &events) != nil || len(events) != 3 {
		t.Errorf("json output = %q, %v", out, err)
	}
}

func TestHistoryExport(t *testing.T) {
	var seen []string
	srv := historyServer(t, 1, &seen)
	defer srv.Close()

	out, _, err := run(t, srv, "history", "-c", "session", "--export", "csv")
	if err != nil || !strings.Contains(out, "session.sign_in") {
		t.Fatalf("stdout export = %q, %v", out, err)
	}

	path := filepath.Join(t.TempDir(), "sign-ins.csv")
	_, errOut, err := run(t, srv, "history", "-c", "session", "--export", "csv", "--file", path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if !strings.HasPrefix(string(b), "time,category") || st.Mode().Perm() != 0o600 || !strings.Contains(errOut, "wrote") {
		t.Errorf("file export: %q, mode %v, stderr %q", b, st.Mode().Perm(), errOut)
	}

	if _, _, err := run(t, srv, "history", "--since", "1h", "--from", "2026-09-01"); err == nil {
		t.Error("--since and --from together accepted")
	}
}

func TestHistoryServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"this needs the operator role"}`))
	}))
	defer srv.Close()
	if _, _, err := run(t, srv, "history"); err == nil || !strings.Contains(err.Error(), "needs the operator role") {
		t.Errorf("err = %v", err)
	}
}
