package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig describes the SceneID client registration.
type OIDCConfig struct {
	Issuer                string
	ClientID              string
	ClientSecret          string
	RedirectURL           string
	PostLogoutRedirectURL string // optional; enables SceneID sign-out
	Scopes                []string
	Roles                 RoleMapping
}

// Errors the callback can end with; the UI shows a message for each.
var (
	ErrUnknownState = errors.New("sign-in attempt expired or unknown")
	ErrNoRole       = errors.New("account has no ForgeSync role")
)

// OIDC runs the authorization code flow with PKCE against SceneID.
//
// Discovery happens on first use, not at startup, so the controller keeps
// syncing Forgejo when SceneID is down; only sign-in fails.
type OIDC struct {
	cfg  OIDCConfig
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	endSess  string
	attempts map[string]attempt
}

// attempt is one sign-in in progress, keyed by its state value.
type attempt struct {
	nonce    string
	verifier string
	returnTo string
	expires  time.Time
}

const (
	attemptTTL  = 10 * time.Minute
	maxAttempts = 10000 // bounds memory if someone floods /auth/login
)

func NewOIDC(cfg OIDCConfig) *OIDC {
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &OIDC{
		cfg:      cfg,
		http:     &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		attempts: map[string]attempt{},
	}
}

func (o *OIDC) setup(ctx context.Context) (*oidc.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil {
		return o.provider, nil
	}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, o.http), o.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("SceneID discovery: %w", err)
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = p.Claims(&extra)
	o.provider, o.endSess = p, extra.EndSession
	return p, nil
}

func (o *OIDC) oauth2Config(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     o.cfg.ClientID,
		ClientSecret: o.cfg.ClientSecret,
		RedirectURL:  o.cfg.RedirectURL,
		Endpoint:     p.Endpoint(),
		Scopes:       o.cfg.Scopes,
	}
}

// Start begins a sign-in. It returns the SceneID URL to send the browser to
// and the state value, which the caller must bind to the browser (cookie) and
// hand back to Finish.
func (o *OIDC) Start(ctx context.Context, returnTo string) (authURL, state string, err error) {
	p, err := o.setup(ctx)
	if err != nil {
		return "", "", err
	}
	state, nonce, verifier := random(), random(), oauth2.GenerateVerifier()
	now := o.now()

	o.mu.Lock()
	for k, a := range o.attempts {
		if now.After(a.expires) {
			delete(o.attempts, k)
		}
	}
	if len(o.attempts) >= maxAttempts {
		o.mu.Unlock()
		return "", "", errors.New("too many sign-ins in progress; try again shortly")
	}
	o.attempts[state] = attempt{nonce: nonce, verifier: verifier, returnTo: returnTo, expires: now.Add(attemptTTL)}
	o.mu.Unlock()

	u := o.oauth2Config(p).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	return u, state, nil
}

// Finish completes a sign-in: it exchanges the code, verifies the ID token
// (signature, issuer, audience, expiry, nonce) and maps the user's role.
// It returns the identity, the raw ID token (for sign-out) and where the user
// wanted to go.
func (o *OIDC) Finish(ctx context.Context, state, code string) (Identity, string, string, error) {
	o.mu.Lock()
	a, ok := o.attempts[state]
	delete(o.attempts, state) // single use, whatever happens next
	o.mu.Unlock()
	if !ok || o.now().After(a.expires) {
		return Identity{}, "", "", ErrUnknownState
	}

	p, err := o.setup(ctx)
	if err != nil {
		return Identity{}, "", a.returnTo, err
	}
	ctx = oidc.ClientContext(ctx, o.http)
	tok, err := o.oauth2Config(p).Exchange(ctx, code, oauth2.VerifierOption(a.verifier))
	if err != nil {
		return Identity{}, "", a.returnTo, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, "", a.returnTo, errors.New("SceneID returned no ID token")
	}
	idt, err := p.VerifierContext(ctx, &oidc.Config{ClientID: o.cfg.ClientID, Now: o.now}).Verify(ctx, raw)
	if err != nil {
		return Identity{}, "", a.returnTo, fmt.Errorf("ID token: %w", err)
	}
	if idt.Nonce != a.nonce {
		return Identity{}, "", a.returnTo, errors.New("ID token: nonce mismatch")
	}

	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, "", a.returnTo, fmt.Errorf("ID token claims: %w", err)
	}
	id := Identity{
		Subject:  idt.Subject,
		Username: claimString(claims, "preferred_username", "nickname", "email"),
		Name:     claimString(claims, "name", "preferred_username", "nickname"),
		Email:    claimString(claims, "email"),
		Role:     o.cfg.Roles.RoleFor(claims),
		Source:   "sceneid",
	}
	if id.Username == "" {
		id.Username = id.Subject
	}
	if id.Role == NoRole {
		return id, "", a.returnTo, ErrNoRole
	}
	return id, raw, a.returnTo, nil
}

// LogoutURL is where to send the browser to end the SceneID session too, or
// "" if SceneID has no end-session endpoint or no post-logout URL is set.
func (o *OIDC) LogoutURL(idToken string) string {
	o.mu.Lock()
	end := o.endSess
	o.mu.Unlock()
	if end == "" || o.cfg.PostLogoutRedirectURL == "" {
		return ""
	}
	u, err := url.Parse(end)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_id", o.cfg.ClientID)
	q.Set("post_logout_redirect_uri", o.cfg.PostLogoutRedirectURL)
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
