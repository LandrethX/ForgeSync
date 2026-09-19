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
	Controller  Controller  `yaml:"controller"`
	Log         Log         `yaml:"log"`
	HTTP        HTTP        `yaml:"http"`
	Database    Database    `yaml:"database"`
	Health      Health      `yaml:"health"`
	Inventory   Inventory   `yaml:"inventory"`
	Replication Replication `yaml:"replication"`
	Webhooks    Webhooks    `yaml:"webhooks"`
	Nodes       []Node      `yaml:"nodes"`
}

// Controller is this controller's own identity in a ForgeSync installation.
// Two controllers can share a database: one holds the leadership lease and
// does the work, the other stands by and takes over when the lease runs
// out. Running a single controller needs none of this.
type Controller struct {
	// Name says which controller this is, in the UI and the log. Default:
	// the host name.
	Name string `yaml:"name"`
	// URL is where people reach this controller, so the standby's UI can
	// send them to the leader. Optional.
	URL string `yaml:"url"`
	// Lease is how long leadership lasts without a renewal: the longest a
	// failover takes, and the longest a controller that has lost the
	// database keeps acting. Default 15s.
	Lease time.Duration `yaml:"lease"`
	// Renew is how often the leader renews it. Default: a third of Lease.
	Renew time.Duration `yaml:"renew"`
	// Priority says which controller should be the one acting when more
	// than one could: the lowest number leads, and a controller that took
	// the lease while a better one was away hands it back when it returns.
	// 0 (the default) means no preference: whoever holds it keeps it.
	Priority int `yaml:"priority"`
}

// Webhooks makes every node report changes to ForgeSync as they happen,
// through a Forgejo system webhook that ForgeSync installs and maintains. Off
// while URL is empty. With it on, inventory.interval can be much longer
// (e.g. 30m to 1h): scans become a safety net.
type Webhooks struct {
	// URL is how the nodes reach the controller's webhook endpoint, e.g.
	// https://sync.scenegit.org/api/v1/hooks/forgejo; each node posts to
	// <url>/<node name>.
	URL string `yaml:"url"`
	// SecretFile holds the secret each node's signing secret is derived from
	// (at least 32 characters).
	SecretFile string `yaml:"secret_file"`
	Secret     string `yaml:"-"`
	// CheckInterval is how often the hooks are checked and repaired.
	// Default 10m.
	CheckInterval time.Duration `yaml:"check_interval"`
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
	// CreateMissing creates a repository on a node that doesn't have it (and
	// its owner, if a SceneID user). Default true.
	CreateMissing *bool `yaml:"create_missing"`
	// AutoFix applies the conflict fixes that lose nothing (a replica's new
	// commits or refs go to the primary; default branches follow the
	// primary). Default true.
	AutoFix *bool `yaml:"auto_fix"`
	// HandOffConflicts opens a pull request on the primary for each diverged
	// branch, for the repository's owner to decide. Default true.
	HandOffConflicts *bool `yaml:"hand_off_conflicts"`
	// BackupDays is how long ForgeSync keeps what it takes away: a
	// replica's version after the owner chose the primary's, and the archived
	// copies of a repository deleted on its primary. Default 30.
	BackupDays int `yaml:"backup_days"`
	// Issues replicates issues and their comments (title, body, state,
	// labels, milestone, assignees) in both directions, merged per field,
	// along with the repositories' own labels and milestones. Off by default.
	Issues bool `yaml:"issues"`
	// Reactions also replicates the reactions on issues and comments, as
	// the people who made them. Forgejo has no bulk endpoint for reactions,
	// so this costs one API call per issue and per comment per node on
	// every run; off by default, and it needs Issues.
	Reactions bool `yaml:"reactions"`
	// Attachments also replicates the files on issues and comments, copied
	// as the author. It costs the same listing per issue and per comment
	// per node, plus a download and an upload for each file that has to
	// move; off by default, and it needs Issues.
	Attachments bool `yaml:"attachments"`
	// AttachmentMaxBytes is the largest file replication carries. A bigger
	// one is left where it is, with a warning. Default 16 MiB.
	AttachmentMaxBytes int64 `yaml:"attachment_max_bytes"`
	// PullRequests takes pull requests in as well: their conversation and
	// their title and body, merged as an issue's are. Their state is not:
	// a copy is closed once the primary's is closed or merged, and
	// ForgeSync never merges or reopens one. A copy is opened on a node
	// only once the branches it is between are there. Off by default, and
	// it needs Issues.
	PullRequests bool `yaml:"pull_requests"`
	// Reviews also replicates what people said about a pull request's diff:
	// each submitted review with its line comments, as the person who wrote
	// it. The line comments carry because the diff is the same everywhere.
	// Needs PullRequests; off by default.
	Reviews bool `yaml:"reviews"`
	// Collaborators keeps the people a repository is shared with, and what
	// each may do, the same on every node. It is what lets an assignee be
	// set on a replica at all. Off by default; it needs no other option.
	Collaborators bool `yaml:"collaborators"`
	// Organizations keeps the organizations that own repositories the same
	// on every node: the organization itself, its profile, its teams and
	// who is in them. Without it, a repository owned by an organization a
	// node hasn't got can't be copied there at all. Off by default.
	Organizations bool `yaml:"organizations"`
	// ProtectReplicas puts ForgeSync's own guard on every replica, so a
	// replica can't be pushed to and people work on the primary. Phase 0
	// (p02) found a repository's owner can delete a protection rule, so
	// ForgeSync puts it back whenever it's gone or has been weakened. Off
	// by default: it changes what users may do.
	ProtectReplicas bool `yaml:"protect_replicas"`
	// BranchProtection keeps the owner's own protection rules the same on
	// every node. Off by default.
	BranchProtection bool `yaml:"branch_protection"`
	// Metadata keeps a repository's settings -- description, website, the
	// units it offers, its merge styles -- and its topics the same on every
	// node. The default branch isn't among them (replication already
	// follows the primary's) and neither is archived. A repository private
	// anywhere becomes private everywhere; the other way is a person's
	// decision. Off by default.
	Metadata bool `yaml:"metadata"`
	// Releases keeps what was published on each tag, and the files with
	// it, the same on every node. A release is identified by its tag, which
	// git replication has already put everywhere. Files are bounded by
	// AttachmentMaxBytes. Off by default.
	Releases bool `yaml:"releases"`
	// Wiki replicates each repository's wiki -- a second git repository --
	// from its primary to the replicas, the way the repository itself is
	// replicated. A node that has no wiki yet gets one page written through
	// the API so Forgejo makes the repository, which is then replaced by
	// the primary's history. Off by default.
	Wiki bool `yaml:"wiki"`
	// Packages copies what the nodes' registries hold, for the package
	// types whose files can be fetched and published by path alone
	// (generic and maven). A package of any other type that isn't on every
	// node is reported rather than quietly left behind.
	// PackageMaxBytes bounds one file; 0 (the default) means no limit.
	// Off by default.
	Packages        bool  `yaml:"packages"`
	PackageMaxBytes int64 `yaml:"package_max_bytes"`
	// LFS copies the Git LFS objects a repository's pointer files name to
	// every node that has the repository, so a replica can be checked out
	// at all. Objects are only ever added, never deleted. LFSMaxBytes
	// bounds one object; 0 (the default) means no limit. Off by default.
	LFS         bool  `yaml:"lfs"`
	LFSMaxBytes int64 `yaml:"lfs_max_bytes"`
	// Actions keeps a repository's Actions variables the same on every
	// node, and reports a node that hasn't got a secret the others have.
	// A secret's value is never given back by Forgejo, so nothing can copy
	// one; the workflows themselves are files, so git already carries them.
	// Off by default.
	Actions bool `yaml:"actions"`
	// ArchiveOrg is the private organization ForgeSync moves the copies of a
	// repository deleted on its primary into. Default "forgesync-archive".
	ArchiveOrg string `yaml:"archive_org"`
}

// Inventory controls the periodic repository scan of every node.
type Inventory struct {
	Interval time.Duration `yaml:"interval"`
	// BranchConcurrency limits parallel branch lookups per node.
	BranchConcurrency int `yaml:"branch_concurrency"`
}

// RoleNames lists the SceneID role/group values that grant each ForgeSync role.

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
	// SceneIDSourceID is the id of the SceneID login source on this node
	// (`forgejo admin auth list`). Needed to create users here.
	SceneIDSourceID int64  `yaml:"sceneid_source_id"`
	Token           string `yaml:"-"`
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
	if c.Controller.Name == "" {
		if host, err := os.Hostname(); err == nil {
			c.Controller.Name = host
		} else {
			c.Controller.Name = "forgesync"
		}
	}
	if c.Controller.Lease == 0 {
		c.Controller.Lease = 15 * time.Second
	}
	if c.Controller.Renew == 0 {
		c.Controller.Renew = c.Controller.Lease / 3
	}
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
	for _, b := range []**bool{&c.Replication.CreateMissing, &c.Replication.AutoFix, &c.Replication.HandOffConflicts} {
		if *b == nil {
			yes := true
			*b = &yes
		}
	}
	if c.Replication.BackupDays == 0 {
		c.Replication.BackupDays = 30
	}
	if c.Webhooks.CheckInterval == 0 {
		c.Webhooks.CheckInterval = 10 * time.Minute
	}
	if c.Replication.ArchiveOrg == "" {
		c.Replication.ArchiveOrg = "forgesync-archive"
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
	if c.Webhooks.SecretFile != "" {
		if c.Webhooks.Secret, err = readSecret(dir, c.Webhooks.SecretFile); err != nil {
			return fmt.Errorf("webhooks.secret_file: %w", err)
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
	if c.Controller.Lease < 2*time.Second {
		errs = append(errs, errors.New("controller.lease must be at least 2s"))
	}
	if c.Controller.Renew <= 0 || c.Controller.Renew >= c.Controller.Lease {
		errs = append(errs, errors.New("controller.renew must be positive and shorter than controller.lease"))
	}
	if c.Controller.URL != "" {
		if u, err := url.Parse(c.Controller.URL); err != nil || !u.IsAbs() {
			errs = append(errs, fmt.Errorf("controller.url %q: want an absolute URL", c.Controller.URL))
		}
	}
	if c.Replication.Reactions && !c.Replication.Issues {
		errs = append(errs, errors.New("replication.reactions needs replication.issues"))
	}
	if c.Replication.Attachments && !c.Replication.Issues {
		errs = append(errs, errors.New("replication.attachments needs replication.issues"))
	}
	if c.Replication.PullRequests && !c.Replication.Issues {
		errs = append(errs, errors.New("replication.pull_requests needs replication.issues"))
	}
	if c.Replication.Reviews && !c.Replication.PullRequests {
		errs = append(errs, errors.New("replication.reviews needs replication.pull_requests"))
	}
	if c.Replication.AttachmentMaxBytes < 0 {
		errs = append(errs, errors.New("replication.attachment_max_bytes must not be negative"))
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
	if c.Inventory.Interval < 10*time.Second {
		errs = append(errs, errors.New("inventory.interval must be at least 10s"))
	}
	if c.Replication.Concurrency < 1 || c.Replication.Concurrency > 16 {
		errs = append(errs, errors.New("replication.concurrency must be 1 to 16"))
	}
	if c.Webhooks.URL != "" {
		if u, err := url.Parse(c.Webhooks.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("webhooks.url must be an http(s) URL"))
		}
		if len(c.Webhooks.Secret) < 32 {
			errs = append(errs, errors.New("webhooks.secret_file must hold at least 32 characters"))
		}
		if c.Webhooks.CheckInterval < time.Minute {
			errs = append(errs, errors.New("webhooks.check_interval must be at least 1m"))
		}
	}
	if c.Replication.BackupDays < 1 {
		errs = append(errs, errors.New("replication.backup_days must be at least 1"))
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
