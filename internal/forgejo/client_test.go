package forgejo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCurrentUserSendsTokenAndSudo(t *testing.T) {
	var gotAuth, gotSudo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotSudo = r.Header.Get("Authorization"), r.Header.Get("Sudo")
		if r.URL.Path != "/api/v1/user" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":7,"login":"bob","is_admin":false,"source_id":2,"login_name":"sub-123"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL+"/", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.Sudo("bob").CurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "token secret" || gotSudo != "bob" {
		t.Errorf("headers: Authorization=%q Sudo=%q", gotAuth, gotSudo)
	}
	if u.Login != "bob" || u.LoginName != "sub-123" || u.SourceID != 2 {
		t.Errorf("user = %+v", u)
	}

	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotSudo != "" {
		t.Errorf("Sudo leaked into the original client: %q", gotSudo)
	}
}

func TestPublicEndpointsSendNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("%s sent a token", r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/healthz":
			w.Write([]byte(`{"status":"pass","description":"Forgejo: Beyond coding. We forge."}`))
		case "/api/v1/version":
			w.Write([]byte(`{"version":"16.0.5+gitea-1.22.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "secret", nil)
	h, err := c.Healthz(context.Background())
	if err != nil || h.Status != "pass" {
		t.Errorf("healthz = %+v, %v", h, err)
	}
	v, err := c.Version(context.Background())
	if err != nil || v != "16.0.5+gitea-1.22.0" {
		t.Errorf("version = %q, %v", v, err)
	}
}

func TestAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"token is required"}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "bad", nil)
	_, err := c.CurrentUser(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || apiErr.Message != "token is required" {
		t.Fatalf("err = %#v", err)
	}
	if !IsAuthError(err) {
		t.Error("IsAuthError = false for a 401")
	}
	if IsAuthError(&APIError{StatusCode: 500}) {
		t.Error("IsAuthError = true for a 500")
	}
}

func TestListReposAndBranchHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/search":
			q := r.URL.Query()
			if q.Get("page") != "2" || q.Get("limit") != "50" || q.Get("sort") != "id" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			w.Header().Set("X-Total-Count", "51")
			w.Write([]byte(`{"ok":true,"data":[{"id":51,"full_name":"alice/demo","owner":{"login":"alice"},"name":"demo","private":true,"default_branch":"main","updated_at":"2026-09-18T09:00:00Z"}]}`))
		case "/api/v1/repos/alice/my repo/branches/feature/x":
			if r.URL.EscapedPath() != "/api/v1/repos/alice/my%20repo/branches/feature/x" {
				t.Errorf("escaped path = %q", r.URL.EscapedPath())
			}
			w.Write([]byte(`{"name":"feature/x","commit":{"id":"abc123"}}`))
		default:
			t.Errorf("unexpected path %q (raw %q)", r.URL.Path, r.URL.RawPath)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "tok", nil)

	repos, total, err := c.ListRepos(context.Background(), 2, 50)
	if err != nil || total != 51 || len(repos) != 1 || repos[0].FullName != "alice/demo" || !repos[0].Private || repos[0].Owner.Login != "alice" {
		t.Fatalf("repos = %+v, total %d, err %v", repos, total, err)
	}
	// Slashes in branch names stay path separators; other characters are escaped.
	sha, err := c.BranchHead(context.Background(), "alice", "my repo", "feature/x")
	if err != nil || sha != "abc123" {
		t.Fatalf("head = %q, err %v", sha, err)
	}
}

func TestCommitsAhead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("files") != "false" || r.URL.Query().Get("verification") != "false" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/api/v1/repos/alice/demo/compare/aaa...bbb":
			w.Write([]byte(`{"total_commits":3,"commits":[]}`))
		case "/api/v1/repos/alice/demo/compare/aaa...ccc":
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"could not find 'ccc' to be a commit, branch or tag"}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "tok", nil)
	ctx := context.Background()

	if n, found, err := c.CommitsAhead(ctx, "alice", "demo", "aaa", "bbb"); n != 3 || !found || err != nil {
		t.Errorf("aaa...bbb = %d %t %v", n, found, err)
	}
	if _, found, err := c.CommitsAhead(ctx, "alice", "demo", "aaa", "ccc"); found || err != nil {
		t.Errorf("unknown commit: found %t err %v", found, err)
	}
	if _, _, err := c.CommitsAhead(ctx, "alice", "demo", "x", "y"); err == nil {
		t.Error("server error not reported")
	}
}
