package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/api"
	"scenegit.org/forgesync/internal/health"
)

func TestNodeList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"missing or invalid bearer token"}`))
			return
		}
		w.Write([]byte(`[{"name":"se","url":"http://forgejo-se.test:3001","site":"SE","node":"se","state":"HEALTHY","version":"16.0.5"}]`))
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "tok", "node", "list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "se    SE    HEALTHY  16.0.5") {
		t.Errorf("output:\n%s", out.String())
	}

	cmd = NewRootCommand(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "wrong", "node", "list"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "invalid bearer token") {
		t.Errorf("err = %v, want the server's message", err)
	}
}

func TestPrintNodesLastSeen(t *testing.T) {
	now := time.Date(2026, 9, 18, 9, 48, 12, 0, time.UTC)
	seen := now.Add(-3*time.Minute - 35*time.Second)
	var out bytes.Buffer
	printNodes(&out, []api.Node{
		{NodeInfo: api.NodeInfo{Name: "dk", Site: "DK"}, Status: health.Status{State: health.Unreachable, LastSeen: &seen}},
		{NodeInfo: api.NodeInfo{Name: "de"}, Status: health.Status{State: health.Unknown}},
	}, now)
	if !strings.Contains(out.String(), "3m35s ago") || !strings.Contains(out.String(), "never") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestRepoSetPrimary(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/repositories":
			if r.URL.Query().Get("q") != "alice/Demo" {
				t.Errorf("q = %q", r.URL.Query().Get("q"))
			}
			// The search is a substring match; the CLI must pick the exact name.
			w.Write([]byte(`{"total":2,"items":[{"id":"id-2","full_name":"alice/demo-old"},{"id":"id-1","full_name":"alice/demo"}]}`))
		case r.Method == "PUT" && r.URL.Path == "/api/v1/repositories/id-1/primary":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.Write([]byte(`{"primary_node":"dk","previous":"se"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "tok", "repo", "set-primary", "alice/Demo", "dk"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"node":"dk"}` || !strings.Contains(out.String(), "alice/Demo: primary dk (was se)") {
		t.Errorf("body %q, output %q", gotBody, out.String())
	}

	cmd = NewRootCommand(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "tok", "repo", "set-primary", "alice/Demo", "-"})
	cmd.Execute()
	if gotBody != `{"node":""}` {
		t.Errorf("clearing sent %q", gotBody)
	}
}

func TestConflictList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "all" {
			t.Errorf("state = %q", r.URL.Query().Get("state"))
		}
		w.Write([]byte(`{"total":1,"counts":{"open":1,"cleared":4},"items":[{"id":7,"full_name":"alice/demo","kind":"git_diverged","state":"open",
			"detected_at":"2026-09-18T10:00:00Z","details":{"heads":{"se":"a1b2c3d4e5","dk":"9f8e7d6c5b"}}}]}`))
	}))
	defer srv.Close()
	var out bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "tok", "conflict", "list", "--state", "all"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice/demo", "git_diverged", "dk:9f8e7d6 se:a1b2c3d", "1 open, 4 cleared"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
