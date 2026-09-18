package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUserCommands(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/users":
			w.Write([]byte(`{"total":2,"items":[
				{"id":"u-2","login":"alice2","home_node":"se","home_source":"registration","accounts":[]},
				{"id":"u-1","login":"alice","home_node":"dk","home_source":"registration","accounts":[
					{"node":"dk","login":"alice","present":true},
					{"node":"se","login":"alice","present":true,"created_by_forgesync":true},
					{"node":"de","login":"alice","present":false}]}]}`))
		case r.Method == "PUT" && r.URL.Path == "/api/v1/users/u-1/home":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.Write([]byte(`{"home_node":"se","previous":"dk"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "tok", "user", "list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alice   dk            registration  dk se(copy)") {
		t.Errorf("list output:\n%s", out.String())
	}

	out.Reset()
	cmd = NewRootCommand(&out)
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "tok", "user", "set-home", "Alice", "se"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"node":"se"}` || !strings.Contains(out.String(), "Alice: primary site se (was dk)") {
		t.Errorf("body %q, output %q", gotBody, out.String())
	}
}
