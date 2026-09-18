package inventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// Lister is the part of the Forgejo client the scanner needs.
type Lister interface {
	ListRepos(ctx context.Context, page, limit int) ([]forgejo.Repository, int, error)
	BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
	ListUsers(ctx context.Context, sourceID int64, page, limit int) ([]forgejo.User, int, error)
	GetRepo(ctx context.Context, owner, name string) (forgejo.Repository, bool, error)
}

// Recorder stores scan results.
type Recorder interface {
	RecordNodeScan(ctx context.Context, node string, started, finished time.Time, repos []store.ScannedRepo) error
	RecordNodeScanFailure(ctx context.Context, node string, started, finished time.Time, err error) error
	RecordNodeUsers(ctx context.Context, node string, finished time.Time, users []store.ScannedUser) error
	RecordRepoObservation(ctx context.Context, node string, at time.Time, fullName string, found bool, r store.ScannedRepo) error
}

type Target struct {
	Name   string
	Client Lister
	// SceneIDSourceID is the SceneID login source on this node; its users
	// are listed too. 0 = don't list users.
	SceneIDSourceID int64
}

type Options struct {
	Interval          time.Duration
	BranchConcurrency int           // parallel branch lookups per node
	NodeTimeout       time.Duration // upper bound for scanning one node
	// AfterScan, if set, runs after every scan of all nodes (e.g. conflict
	// detection, which needs every node's latest view).
	AfterScan func(ctx context.Context)
	// SkipOwners are owners whose repositories aren't inventoried, e.g.
	// ForgeSync's archive organization.
	SkipOwners []string
}

// pageSize matches Forgejo's default MAX_RESPONSE_ITEMS.
const pageSize = 50

// Scanner lists every repository on every node on an interval, or when
// triggered, and records what it finds.
type Scanner struct {
	targets []Target
	opts    Options
	rec     Recorder
	log     *slog.Logger
	now     func() time.Time

	trigger chan struct{}
	running atomic.Bool
}

func NewScanner(targets []Target, opts Options, rec Recorder, log *slog.Logger) *Scanner {
	if opts.BranchConcurrency < 1 {
		opts.BranchConcurrency = 4
	}
	if opts.NodeTimeout == 0 {
		opts.NodeTimeout = 10 * time.Minute
	}
	return &Scanner{targets: targets, opts: opts, rec: rec, log: log, now: time.Now, trigger: make(chan struct{}, 1)}
}

// Interval is the time between scheduled scans.
func (s *Scanner) Interval() time.Duration { return s.opts.Interval }

// Running reports whether a scan is in progress.
func (s *Scanner) Running() bool { return s.running.Load() }

// Trigger asks for a scan as soon as possible. It returns false if one is
// already running or queued.
func (s *Scanner) Trigger() bool {
	if s.running.Load() {
		return false
	}
	select {
	case s.trigger <- struct{}{}:
		return true
	default:
		return false
	}
}

// Run scans immediately, then on every interval or trigger, until ctx ends.
func (s *Scanner) Run(ctx context.Context) {
	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()
	for {
		s.ScanAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.trigger:
			ticker.Reset(s.opts.Interval)
		}
	}
}

// ScanAll scans every node in parallel and records the results.
func (s *Scanner) ScanAll(ctx context.Context) {
	s.running.Store(true)
	defer s.running.Store(false)
	var wg sync.WaitGroup
	for _, t := range s.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := s.now().UTC()
			repos, err := s.scanNode(ctx, t)
			var users []store.ScannedUser
			if err == nil && t.SceneIDSourceID != 0 {
				users, err = s.scanUsers(ctx, t)
			}
			finished := s.now().UTC()
			if ctx.Err() != nil {
				return // shutting down; don't record a half scan
			}
			if err != nil {
				s.log.Warn("inventory scan failed", "node", t.Name, "error", err)
				if rerr := s.rec.RecordNodeScanFailure(ctx, t.Name, started, finished, err); rerr != nil {
					s.log.Error("recording scan failure failed", "node", t.Name, "error", rerr)
				}
				return
			}
			// Users first: the scan only counts as successful (which primary
			// assignment relies on) once both are recorded.
			if t.SceneIDSourceID != 0 {
				if err := s.rec.RecordNodeUsers(ctx, t.Name, finished, users); err != nil {
					// Record the scan as failed, so this round doesn't count as
					// complete on the strength of an older successful scan.
					s.log.Error("recording users failed", "node", t.Name, "error", err)
					if rerr := s.rec.RecordNodeScanFailure(ctx, t.Name, started, finished, err); rerr != nil {
						s.log.Error("recording scan failure failed", "node", t.Name, "error", rerr)
					}
					return
				}
			}
			if err := s.rec.RecordNodeScan(ctx, t.Name, started, finished, repos); err != nil {
				s.log.Error("recording scan failed", "node", t.Name, "error", err)
				return
			}
			s.log.Info("inventory scan finished", "node", t.Name, "repositories", len(repos), "duration", finished.Sub(started))
		}()
	}
	wg.Wait()
	if s.opts.AfterScan != nil && ctx.Err() == nil {
		s.opts.AfterScan(ctx)
	}
}

// ScanRepo looks at one repository on every node and records what each has,
// without a full scan (e.g. after a webhook or a replication run). A node
// that can't be asked keeps what was recorded before.
func (s *Scanner) ScanRepo(ctx context.Context, fullName string) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || s.skipOwner(owner) {
		return
	}
	var wg sync.WaitGroup
	for _, t := range s.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, found, err := t.Client.GetRepo(ctx, owner, name)
			if err != nil {
				s.log.Warn("checking a repository failed", "node", t.Name, "repository", fullName, "error", err)
				return
			}
			obs := store.ScannedRepo{}
			if found {
				obs = store.ScannedRepo{FullName: r.FullName, ForgejoID: r.ID, Private: r.Private, Fork: r.Fork, Mirror: r.Mirror,
					Archived: r.Archived, Empty: r.Empty, DefaultBranch: r.DefaultBranch, Updated: r.Updated, Created: r.Created}
				if !r.Empty && r.DefaultBranch != "" {
					if sha, err := t.Client.BranchHead(ctx, r.Owner.Login, r.Name, r.DefaultBranch); err != nil {
						obs.HeadError = err.Error()
					} else {
						obs.HeadSHA = sha
					}
				}
			}
			if err := s.rec.RecordRepoObservation(ctx, t.Name, s.now().UTC(), fullName, found, obs); err != nil {
				s.log.Error("recording a repository failed", "node", t.Name, "repository", fullName, "error", err)
			}
		}()
	}
	wg.Wait()
}

// scanUsers lists a node's SceneID accounts.
func (s *Scanner) scanUsers(ctx context.Context, t Target) ([]store.ScannedUser, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.NodeTimeout)
	defer cancel()
	var out []store.ScannedUser
	seen := map[int64]bool{}
	for page, total := 1, -1; ; page++ {
		us, n, err := t.Client.ListUsers(ctx, t.SceneIDSourceID, page, pageSize)
		if err != nil {
			return nil, fmt.Errorf("list users (page %d): %w", page, err)
		}
		if total < 0 {
			total = n
		}
		for _, u := range us {
			if !seen[u.ID] && u.LoginName != "" {
				seen[u.ID] = true
				out = append(out, store.ScannedUser{Login: u.Login, ForgejoID: u.ID, Sub: u.LoginName, Created: u.Created})
			}
		}
		if len(us) < pageSize || len(seen) >= total || page > total/pageSize+2 {
			return out, nil
		}
	}
}

func (s *Scanner) skipOwner(login string) bool {
	for _, o := range s.opts.SkipOwners {
		if strings.EqualFold(o, login) {
			return true
		}
	}
	return false
}

func (s *Scanner) scanNode(ctx context.Context, t Target) ([]store.ScannedRepo, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.NodeTimeout)
	defer cancel()

	var all []forgejo.Repository
	seen := map[int64]bool{}
	for page, total := 1, -1; ; page++ {
		repos, n, err := t.Client.ListRepos(ctx, page, pageSize)
		if err != nil {
			return nil, fmt.Errorf("list repositories (page %d): %w", page, err)
		}
		if total < 0 {
			total = n
		}
		for _, r := range repos {
			// Repositories created while paging can shift pages; skip repeats.
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if !s.skipOwner(r.Owner.Login) {
				all = append(all, r)
			}
		}
		if len(repos) < pageSize || len(seen) >= total || page > total/pageSize+2 {
			break
		}
	}

	out := make([]store.ScannedRepo, len(all))
	sem := make(chan struct{}, s.opts.BranchConcurrency)
	var wg sync.WaitGroup
	for i, r := range all {
		out[i] = store.ScannedRepo{
			FullName: r.FullName, ForgejoID: r.ID, Private: r.Private, Fork: r.Fork, Mirror: r.Mirror,
			Archived: r.Archived, Empty: r.Empty, DefaultBranch: r.DefaultBranch, Updated: r.Updated,
			Created: r.Created,
		}
		if r.Empty || r.DefaultBranch == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			sha, err := t.Client.BranchHead(ctx, r.Owner.Login, r.Name, r.DefaultBranch)
			if err != nil {
				out[i].HeadError = err.Error()
				return
			}
			out[i].HeadSHA = sha
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(errors.New("scan timed out or was cancelled"), err)
	}
	return out, nil
}
