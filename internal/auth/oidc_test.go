package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeSceneID is a minimal OIDC provider: discovery, JWKS, and a token
// endpoint that checks the PKCE verifier and returns a signed ID token.
type fakeSceneID struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	challenge string         // from the authorization URL
	nonce     string         // from the authorization URL
	claims    map[string]any // extra ID token claims
	audience  string
	badNonce  bool
}

func newFakeSceneID(t *testing.T) *fakeSceneID {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSceneID{t: t, key: key, audience: "forgesync-admin"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/auth",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/jwks",
			"end_session_endpoint":                  f.srv.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("/token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSceneID) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	nonce := f.nonce
	if f.badNonce {
		nonce = "something-else"
	}
	claims := map[string]any{
		"iss":   f.srv.URL,
		"sub":   "3f1c-alice",
		"aud":   f.audience,
		"exp":   time.Now().Add(5 * time.Minute).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": nonce,
	}
	for k, v := range f.claims {
		claims[k] = v
	}
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at", "token_type": "Bearer", "expires_in": 300, "id_token": idToken,
	})
}

// authorize plays the browser + SceneID login: remembers what the auth URL
// asked for, as the real provider would bind it to the code.
func (f *fakeSceneID) authorize(t *testing.T, authURL string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" ||
		q.Get("client_id") != "forgesync-admin" || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("authorization URL missing PKCE/nonce/client/scope: %s", authURL)
	}
	f.mu.Lock()
	f.challenge, f.nonce = q.Get("code_challenge"), q.Get("nonce")
	f.mu.Unlock()
}

func newTestOIDC(f *fakeSceneID) *OIDC {
	return NewOIDC(OIDCConfig{
		Issuer:                f.srv.URL,
		ClientID:              "forgesync-admin",
		ClientSecret:          "secret",
		RedirectURL:           "http://127.0.0.1:8090/api/v1/auth/callback",
		PostLogoutRedirectURL: "http://127.0.0.1:8090/",
		Roles: RoleMapping{
			Claim:         "roles",
			Administrator: []string{"forgesync-admin"},
			Operator:      []string{"forgesync-operator"},
			Viewer:        []string{"forgesync-viewer"},
		},
	})
}

func TestSignInFlow(t *testing.T) {
	f := newFakeSceneID(t)
	f.claims = map[string]any{
		"preferred_username": "alice", "name": "Alice Andersson", "email": "alice@sceneid.test",
		"roles": []string{"offline_access", "forgesync-operator"},
	}
	o := newTestOIDC(f)
	ctx := context.Background()

	authURL, state, err := o.Start(ctx, "/nodes/se")
	if err != nil {
		t.Fatal(err)
	}
	f.authorize(t, authURL)
	id, raw, returnTo, err := o.Finish(ctx, state, "good-code")
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Subject: "3f1c-alice", Username: "alice", Name: "Alice Andersson", Email: "alice@sceneid.test", Role: Operator, Source: "sceneid"}
	if id != want || raw == "" || returnTo != "/nodes/se" {
		t.Fatalf("got %+v, token %t, returnTo %q", id, raw != "", returnTo)
	}
	if id.Actor() != "sceneid:alice" {
		t.Errorf("actor = %q", id.Actor())
	}

	// The state is single use: replaying the callback fails.
	if _, _, _, err := o.Finish(ctx, state, "good-code"); !errors.Is(err, ErrUnknownState) {
		t.Errorf("replayed state: err = %v", err)
	}

	logout := o.LogoutURL(raw)
	if !strings.HasPrefix(logout, f.srv.URL+"/logout?") || !strings.Contains(logout, "id_token_hint=") ||
		!strings.Contains(logout, "post_logout_redirect_uri=http%3A%2F%2F127.0.0.1%3A8090%2F") {
		t.Errorf("logout URL = %s", logout)
	}
}

func TestSignInRejections(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		setup func(*fakeSceneID, *OIDC)
		code  string
		check func(error) bool
	}{
		"no ForgeSync role": {
			setup: func(f *fakeSceneID, _ *OIDC) { f.claims = map[string]any{"roles": []string{"offline_access"}} },
			code:  "good-code",
			check: func(err error) bool { return errors.Is(err, ErrNoRole) },
		},
		"nonce mismatch": {
			setup: func(f *fakeSceneID, _ *OIDC) { f.badNonce = true },
			code:  "good-code",
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), "nonce") },
		},
		"token for another client": {
			setup: func(f *fakeSceneID, _ *OIDC) { f.audience = "some-other-app" },
			code:  "good-code",
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), "ID token") },
		},
		"code rejected by SceneID": {
			code:  "stolen-code",
			check: func(err error) bool { return err != nil && strings.Contains(err.Error(), "code exchange") },
		},
		"attempt expired": {
			setup: func(_ *fakeSceneID, o *OIDC) {
				o.now = func() time.Time { return time.Now().Add(attemptTTL + time.Minute) }
			},
			code:  "good-code",
			check: func(err error) bool { return errors.Is(err, ErrUnknownState) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeSceneID(t)
			f.claims = map[string]any{"roles": []string{"forgesync-admin"}}
			o := newTestOIDC(f)
			authURL, state, err := o.Start(ctx, "/")
			if err != nil {
				t.Fatal(err)
			}
			f.authorize(t, authURL)
			if tc.setup != nil {
				tc.setup(f, o)
			}
			if _, _, _, err := o.Finish(ctx, state, tc.code); !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestUnreachableSceneIDRetriesLater(t *testing.T) {
	f := newFakeSceneID(t)
	o := newTestOIDC(f)
	o.cfg.Issuer = "http://127.0.0.1:1"
	if _, _, err := o.Start(context.Background(), "/"); err == nil {
		t.Fatal("Start succeeded without SceneID")
	}
	o.cfg.Issuer = f.srv.URL
	if _, _, err := o.Start(context.Background(), "/"); err != nil {
		t.Fatalf("Start after SceneID came back: %v", err)
	}
}

func TestRoleMapping(t *testing.T) {
	m := RoleMapping{Claim: "realm_access.roles", Administrator: []string{"fs-admin"}, Viewer: []string{"fs-view", "staff"}}
	claims := func(roles ...any) map[string]any {
		return map[string]any{"realm_access": map[string]any{"roles": roles}}
	}
	if r := m.RoleFor(claims("staff", "fs-admin")); r != Administrator {
		t.Errorf("highest role should win, got %v", r)
	}
	if r := m.RoleFor(claims("staff")); r != Viewer {
		t.Errorf("got %v", r)
	}
	if r := m.RoleFor(claims("other")); r != NoRole {
		t.Errorf("got %v", r)
	}
	if r := m.RoleFor(map[string]any{"realm_access": "not an object"}); r != NoRole {
		t.Errorf("malformed claim: got %v", r)
	}
	single := RoleMapping{Claim: "group", Operator: []string{"ops"}}
	if r := single.RoleFor(map[string]any{"group": "ops"}); r != Operator {
		t.Errorf("string claim: got %v", r)
	}
}
