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
	Log      Log      `yaml:"log"`
	HTTP     HTTP     `yaml:"http"`
	Database Database `yaml:"database"`
	Health   Health   `yaml:"health"`
	Nodes    []Node   `yaml:"nodes"`
}

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
	if c.Health.Interval == 0 {
		c.Health.Interval = 15 * time.Second
	}
	if c.Health.Timeout == 0 {
		c.Health.Timeout = 5 * time.Second
	}
	if c.Health.FailureThreshold == 0 {
		c.Health.FailureThreshold = 3
	}
	for i := range c.Nodes {
		if c.Nodes[i].ServiceUser == "" {
			c.Nodes[i].ServiceUser = "forgesync"
		}
	}
}

func (c *Config) resolveSecrets(dir string) error {
	var err error
	if c.HTTP.AdminTokenFile != "" {
		if c.HTTP.AdminToken, err = readSecret(dir, c.HTTP.AdminTokenFile); err != nil {
			return fmt.Errorf("http.admin_token_file: %w", err)
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
