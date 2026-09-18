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
