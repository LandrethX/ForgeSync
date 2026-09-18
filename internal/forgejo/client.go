// Package forgejo is a small client for the Forgejo REST API.
//
// It covers only what ForgeSync uses and grows with it. Check request and
// response fields against the Forgejo v16 source (modules/structs) before
// adding them; Gitea-era docs are often wrong in the details.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/buildinfo"
)

// Client talks to one Forgejo node. It is safe for concurrent use.
type Client struct {
	base  *url.URL
	token string
	sudo  string
	http  *http.Client
}

// New returns a client for the Forgejo instance at baseURL, authenticating
// with an API token.
func New(baseURL, token string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: u, token: token, http: httpClient}, nil
}

// Sudo returns a copy of the client whose requests act as the given user.
// Forgejo allows this only for site-admin tokens.
func (c *Client) Sudo(user string) *Client {
	cp := *c
	cp.sudo = user
	return &cp
}

// APIError is a non-2xx response from Forgejo.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("forgejo %s %s: %d %s", e.Method, e.Path, e.StatusCode, msg)
}

// IsAuthError reports whether err is a 401 or 403 from Forgejo.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

// do sends a request to path (relative to the instance root) and decodes a
// JSON response into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, auth bool, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "forgesync/"+buildinfo.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "token "+c.token)
		if c.sudo != "" {
			req.Header.Set("Sudo", c.sudo)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Method: method, Path: path, StatusCode: resp.StatusCode}
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &m) == nil {
			apiErr.Message = m.Message
		}
		return apiErr
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("forgejo %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Healthz is the response of /api/healthz.
type Healthz struct {
	Status      string `json:"status"` // pass, warn or fail
	Description string `json:"description"`
}

// Healthz checks Forgejo's own health endpoint. It needs no token.
func (c *Client) Healthz(ctx context.Context) (Healthz, error) {
	var h Healthz
	err := c.do(ctx, http.MethodGet, "/api/healthz", false, nil, &h)
	return h, err
}

// Version returns the Forgejo version string, e.g. "16.0.5+gitea-1.22.0".
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/version", false, nil, &v)
	return v.Version, err
}

// User is a Forgejo account. LoginName and SourceID are only filled in for
// admin callers; for SceneID users LoginName holds the OIDC subject.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	FullName  string `json:"full_name"`
	IsAdmin   bool   `json:"is_admin"`
	SourceID  int64  `json:"source_id"`
	LoginName string `json:"login_name"`
}

// CurrentUser returns the account the token (or sudo user) acts as.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/api/v1/user", true, nil, &u)
	return u, err
}
