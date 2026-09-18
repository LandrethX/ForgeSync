package cli

import (
	"bytes"
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
