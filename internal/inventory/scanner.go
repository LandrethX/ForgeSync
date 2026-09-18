package inventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
}

// Recorder stores scan results.
type Recorder interface {
	RecordNodeScan(ctx context.Context, node string, started, finished time.Time, repos []store.ScannedRepo) error
	RecordNodeScanFailure(ctx context.Context, node string, started, finished time.Time, err error) error
}

type Target struct {
	Name   string
	Client Lister
}

type Options struct {
	Interval          time.Duration
	BranchConcurrency int           // parallel branch lookups per node
	NodeTimeout       time.Duration // upper bound for scanning one node
	// AfterScan, if set, runs after every scan of all nodes (e.g. conflict
	// detection, which needs every node's latest view).
	AfterScan func(ctx context.Context)
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
			if !seen[r.ID] {
				seen[r.ID] = true
				all = append(all, r)
			}
		}
		if len(repos) < pageSize || len(all) >= total || page > total/pageSize+2 {
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
