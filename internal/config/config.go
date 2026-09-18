// Package config loads and validates the controller's YAML configuration.
//
// Forgejo tokens and the admin API token are always read from files named in
// the config, so the config file itself can be shared. The database URL may
// be inline for local development; deployments should use database.url_file
// or FORGESYNC_DATABASE_URL.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvDatabaseURL overrides database.url / database.url_file when set.
const EnvDatabaseURL = "FORGESYNC_DATABASE_URL"

type Config struct {
	Log         Log         `yaml:"log"`
	HTTP        HTTP        `yaml:"http"`
	OIDC        OIDC        `yaml:"oidc"`
	Database    Database    `yaml:"database"`
	Health      Health      `yaml:"health"`
	Inventory   Inventory   `yaml:"inventory"`
	Replication Replication `yaml:"replication"`
	Nodes       []Node      `yaml:"nodes"`
}

// Replication copies Git branches and tags from each repository's primary
// to the other nodes. Off unless enabled.
type Replication struct {
	Enabled bool `yaml:"enabled"`
	// WorkDir holds a bare cache per repository; relative paths are resolved
	// against the config file.
	WorkDir     string `yaml:"work_dir"`
	Concurrency int    `yaml:"concurrency"` // repositories in parallel
	Git         string `yaml:"git"`         // git binary
}

// Inventory controls the periodic repository scan of every node.
type Inventory struct {
	Interval time.Duration `yaml:"interval"`
	// BranchConcurrency limits parallel branch lookups per node.
	BranchConcurrency int `yaml:"branch_concurrency"`
}

// OIDC configures SceneID sign-in for the web UI. It's off when Issuer is
// empty; the web UI then uses the admin token instead.
type OIDC struct {
	Issuer           string `yaml:"issuer"`
	ClientID         string `yaml:"client_id"`
	ClientSecretFile string `yaml:"client_secret_file"`
	ClientSecret     string `yaml:"-"`
	// RedirectURL must be this controller's /api/v1/auth/callback as the
	// browser reaches it, and registered with SceneID.
	RedirectURL string `yaml:"redirect_url"`
	// PostLogoutRedirectURL, when set (and registered with SceneID), makes
	// signing out end the SceneID session too.
	PostLogoutRedirectURL string   `yaml:"post_logout_redirect_url"`
	Scopes                []string `yaml:"scopes"`
	// RolesClaim is the ID token claim with the user's roles or groups; dots
	// walk into nested objects (e.g. realm_access.roles).
	RolesClaim string    `yaml:"roles_claim"`
	Roles      RoleNames `yaml:"roles"`
	// AllowTokenSignIn keeps admin-token sign-in in the web UI as a
	// break-glass option while OIDC is on. Every use is audited.
	AllowTokenSignIn bool `yaml:"allow_token_sign_in"`
}

// RoleNames lists the SceneID role/group values that grant each ForgeSync role.
type RoleNames struct {
	Administrator []string `yaml:"administrator"`
	Operator      []string `yaml:"operator"`
	Viewer        []string `yaml:"viewer"`
}

// Enabled reports whether SceneID sign-in is configured.
func (o OIDC) Enabled() bool { return o.Issuer != "" }

type Log struct {
	Level  string `yaml:"level"`  // debug, info, warn, error
	Format string `yaml:"format"` // text, json
}

type HTTP struct {
	Listen string `yaml:"listen"`
	// AdminTokenFile holds the bearer token for /api/v1. Without it the
	// admin API is disabled; /healthz and /readyz stay available.
	AdminTokenFile string `yaml:"admin_token_file"`
	AdminToken     string `yaml:"-"`
	// SecureCookies marks the web UI's session cookie Secure (HTTPS only).
	// Defaults to true; set false only for local development over plain http.
	SecureCookies *bool `yaml:"secure_cookies"`
}

type Database struct {
	URL     string `yaml:"url"`
	URLFile string `yaml:"url_file"`
}

type Health struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// FailureThreshold is how many consecutive failed checks turn a SUSPECT
	// node UNREACHABLE, so a short network blip doesn't count as an outage.
	FailureThreshold int `yaml:"failure_threshold"`
}

type Node struct {
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`
	Site      string `yaml:"site"`
	TokenFile string `yaml:"token_file"`
	// ServiceUser is the local admin account the token must belong to.
	ServiceUser string `yaml:"service_user"`
	Token       string `yaml:"-"`
}

var nodeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Load reads the config file, applies defaults, resolves secret files
// relative to the config file's directory and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.resolveSecrets(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.HTTP.Listen == "" {
		c.HTTP.Listen = "127.0.0.1:8090"
	}
	if c.HTTP.SecureCookies == nil {
		secure := true
		c.HTTP.SecureCookies = &secure
	}
	if c.Health.Interval == 0 {
		c.Health.Interval = 15 * time.Second
	}
	if c.Health.Timeout == 0 {
		c.Health.Timeout = 5 * time.Second
	}
	if c.Health.FailureThreshold == 0 {
		c.Health.FailureThreshold = 3
	}
	if c.Replication.WorkDir == "" {
		c.Replication.WorkDir = "/var/lib/forgesync/git"
	}
	if c.Replication.Concurrency == 0 {
		c.Replication.Concurrency = 2
	}
	if c.Replication.Git == "" {
		c.Replication.Git = "git"
	}
	if c.Inventory.Interval == 0 {
		c.Inventory.Interval = 5 * time.Minute
	}
	if c.Inventory.BranchConcurrency == 0 {
		c.Inventory.BranchConcurrency = 4
	}
	for i := range c.Nodes {
		if c.Nodes[i].ServiceUser == "" {
			c.Nodes[i].ServiceUser = "forgesync"
		}
	}
}

func (c *Config) resolveSecrets(dir string) error {
	var err error
	if !filepath.IsAbs(c.Replication.WorkDir) {
		abs, err := filepath.Abs(filepath.Join(dir, c.Replication.WorkDir))
		if err != nil {
			return fmt.Errorf("replication.work_dir: %w", err)
		}
		c.Replication.WorkDir = abs
	}
	if c.HTTP.AdminTokenFile != "" {
		if c.HTTP.AdminToken, err = readSecret(dir, c.HTTP.AdminTokenFile); err != nil {
			return fmt.Errorf("http.admin_token_file: %w", err)
		}
	}
	if c.OIDC.ClientSecretFile != "" {
		if c.OIDC.ClientSecret, err = readSecret(dir, c.OIDC.ClientSecretFile); err != nil {
			return fmt.Errorf("oidc.client_secret_file: %w", err)
		}
	}
	switch {
	case os.Getenv(EnvDatabaseURL) != "":
		c.Database.URL = os.Getenv(EnvDatabaseURL)
	case c.Database.URLFile != "":
		if c.Database.URL, err = readSecret(dir, c.Database.URLFile); err != nil {
			return fmt.Errorf("database.url_file: %w", err)
		}
	}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.TokenFile == "" {
			return fmt.Errorf("node %q: token_file is required", n.Name)
		}
		if n.Token, err = readSecret(dir, n.TokenFile); err != nil {
			return fmt.Errorf("node %q: token_file: %w", n.Name, err)
		}
	}
	return nil
}

func readSecret(dir, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func (c *Config) validate() error {
	var errs []error
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level))
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format %q: want text or json", c.Log.Format))
	}
	if c.Database.URL == "" {
		errs = append(errs, fmt.Errorf("database: set url, url_file or %s", EnvDatabaseURL))
	}
	if c.Health.Interval < time.Second {
		errs = append(errs, errors.New("health.interval must be at least 1s"))
	}
	if c.Health.Timeout <= 0 || c.Health.Timeout >= c.Health.Interval {
		errs = append(errs, errors.New("health.timeout must be positive and shorter than health.interval"))
	}
	if c.Health.FailureThreshold < 1 {
		errs = append(errs, errors.New("health.failure_threshold must be at least 1"))
	}
	if c.OIDC.Enabled() {
		errs = append(errs, c.OIDC.validate()...)
	}
	if c.Inventory.Interval < 10*time.Second {
		errs = append(errs, errors.New("inventory.interval must be at least 10s"))
	}
	if c.Replication.Concurrency < 1 || c.Replication.Concurrency > 16 {
		errs = append(errs, errors.New("replication.concurrency must be 1 to 16"))
	}
	if c.Inventory.BranchConcurrency < 1 || c.Inventory.BranchConcurrency > 32 {
		errs = append(errs, errors.New("inventory.branch_concurrency must be 1 to 32"))
	}
	if len(c.Nodes) == 0 {
		errs = append(errs, errors.New("nodes: at least one node is required"))
	}
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if !nodeName.MatchString(n.Name) {
			errs = append(errs, fmt.Errorf("node name %q: use lowercase letters, digits and dashes", n.Name))
		}
		if seen[n.Name] {
			errs = append(errs, fmt.Errorf("node name %q is used twice", n.Name))
		}
		seen[n.Name] = true
		u, err := url.Parse(n.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("node %q: url %q must be an absolute http(s) URL", n.Name, n.URL))
		}
	}
	return errors.Join(errs...)
}

func (o OIDC) validate() []error {
	var errs []error
	if u, err := url.Parse(o.Issuer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("oidc.issuer %q must be an absolute http(s) URL", o.Issuer))
	}
	if o.ClientID == "" {
		errs = append(errs, errors.New("oidc.client_id is required"))
	}
	if o.ClientSecret == "" {
		errs = append(errs, errors.New("oidc.client_secret_file is required"))
	}
	if u, err := url.Parse(o.RedirectURL); err != nil || u.Host == "" || !strings.HasSuffix(u.Path, "/api/v1/auth/callback") {
		errs = append(errs, fmt.Errorf("oidc.redirect_url %q must be an absolute URL ending in /api/v1/auth/callback", o.RedirectURL))
	}
	if o.PostLogoutRedirectURL != "" {
		if u, err := url.Parse(o.PostLogoutRedirectURL); err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("oidc.post_logout_redirect_url %q must be an absolute URL", o.PostLogoutRedirectURL))
		}
	}
	if o.RolesClaim == "" {
		errs = append(errs, errors.New("oidc.roles_claim is required"))
	}
	if len(o.Roles.Administrator)+len(o.Roles.Operator)+len(o.Roles.Viewer) == 0 {
		errs = append(errs, errors.New("oidc.roles: map at least one SceneID value to a ForgeSync role, or nobody can sign in"))
	}
	return errs
}
