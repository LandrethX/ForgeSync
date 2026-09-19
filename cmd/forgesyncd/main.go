// Command forgesyncd is the ForgeSync controller.
//
// For now it registers the configured Forgejo nodes, monitors their health
// and serves the admin API. Replication comes after the Phase 0 results.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"scenegit.org/forgesync/internal/api"
	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/config"
	"scenegit.org/forgesync/internal/conflicts"
	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/inventory"
	"scenegit.org/forgesync/internal/issues"
	"scenegit.org/forgesync/internal/leader"
	"scenegit.org/forgesync/internal/replication"
	"scenegit.org/forgesync/internal/store"
	"scenegit.org/forgesync/internal/webhook"
	"scenegit.org/forgesync/internal/webui"
)

func main() {
	configPath := flag.String("config", "/etc/forgesync/forgesync.yaml", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("forgesyncd", buildinfo.String())
		return
	}
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "forgesyncd:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	startedAt := time.Now()
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log)
	log.Info("starting forgesyncd", "version", buildinfo.Version, "commit", buildinfo.Commit, "nodes", len(cfg.Nodes))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer db.Close()
	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	for _, name := range applied {
		log.Info("applied database migration", "migration", name)
	}

	// One controller acts at a time. On its own it simply holds the lease
	// from the start; with a second one, whichever holds it does the work
	// and the other stands by, ready to take over when it runs out.
	holder := leader.NewHolderID()
	elector := leader.New(db, leader.Options{
		Holder: holder, Name: cfg.Controller.Name, URL: cfg.Controller.URL,
		TTL: cfg.Controller.Lease, Renew: cfg.Controller.Renew,
		Yield: yieldToBetterController(db, cfg, log),
	}, log)
	log.Info("controller", "name", cfg.Controller.Name, "url", cfg.Controller.URL,
		"lease", cfg.Controller.Lease, "renew", cfg.Controller.Renew, "priority", cfg.Controller.Priority)

	var records []store.NodeRecord
	var infos []api.NodeInfo
	var targets []health.Target
	var scanTargets []inventory.Target
	var nodeNames []string
	comparers := map[string]conflicts.Comparer{}
	var gitNodes []replication.Node
	var hookTargets []webhook.Target
	var issueNodes []issues.Node
	serviceUsers := map[string]string{}
	for _, n := range cfg.Nodes {
		client, err := forgejo.New(n.URL, n.Token, nil)
		if err != nil {
			return fmt.Errorf("node %s: %w", n.Name, err)
		}
		records = append(records, store.NodeRecord{Name: n.Name, URL: n.URL, Site: n.Site})
		infos = append(infos, api.NodeInfo{Name: n.Name, URL: n.URL, Site: n.Site})
		targets = append(targets, health.Target{Name: n.Name, ServiceUser: n.ServiceUser, Client: client})
		scanTargets = append(scanTargets, inventory.Target{Name: n.Name, Client: client, SceneIDSourceID: n.SceneIDSourceID})
		nodeNames = append(nodeNames, n.Name)
		comparers[n.Name] = client
		hookTargets = append(hookTargets, webhook.Target{Name: n.Name, Client: client})
		issueNodes = append(issueNodes, issues.Node{Name: n.Name, API: client,
			As: func(login string) issues.API { return client.Sudo(login) }})
		serviceUsers[n.Name] = n.ServiceUser
		gitNodes = append(gitNodes, replication.Node{Name: n.Name, URL: n.URL, User: n.ServiceUser, Token: n.Token,
			API: client, As: func(login string) replication.NodeAPI { return client.Sudo(login) },
			SceneIDSourceID: n.SceneIDSourceID})
	}
	if err := db.SyncNodes(ctx, records); err != nil {
		return err
	}

	monitor := health.NewMonitor(targets, health.Options{
		Interval:         cfg.Health.Interval,
		Timeout:          cfg.Health.Timeout,
		FailureThreshold: cfg.Health.FailureThreshold,
	}, leaderRecorder{db, elector.Leading}, log)
	// Start from where the nodes were left, so restarting a controller
	// isn't recorded as every node going from UNKNOWN to HEALTHY. Without
	// this the history fills up with ForgeSync's own restarts and a real
	// change is lost among them.
	if states, err := db.NodeStates(ctx); err != nil {
		log.Warn("reading the nodes' last known state failed; starting from unknown", "error", err)
	} else {
		monitor.Restore(states)
	}
	var scanner *inventory.Scanner
	var engine *replication.Engine
	if cfg.Replication.Enabled {
		git := &replication.Git{Bin: cfg.Replication.Git, WorkDir: cfg.Replication.WorkDir}
		v, err := git.Version(ctx)
		if err != nil {
			return fmt.Errorf("replication is enabled but %w", err)
		}
		engine = replication.NewEngine(gitNodes, git, db, monitor, replication.Options{
			Concurrency:      cfg.Replication.Concurrency,
			CreateMissing:    *cfg.Replication.CreateMissing,
			AutoFix:          *cfg.Replication.AutoFix,
			HandOff:          *cfg.Replication.HandOffConflicts,
			Collaborators:    cfg.Replication.Collaborators,
			Organizations:    cfg.Replication.Organizations,
			ProtectReplicas:  cfg.Replication.ProtectReplicas,
			BranchProtection: cfg.Replication.BranchProtection,
			Metadata:         cfg.Replication.Metadata,
			Releases:         cfg.Replication.Releases,
			Wiki:             cfg.Replication.Wiki,
			Actions:          cfg.Replication.Actions,
			Packages:         cfg.Replication.Packages,
			PackageMax:       cfg.Replication.PackageMaxBytes,
			LFS:              cfg.Replication.LFS,
			LFSMax:           cfg.Replication.LFSMaxBytes,
			AssetMax:         cfg.Replication.AttachmentMaxBytes,
			BackupFor:        time.Duration(cfg.Replication.BackupDays) * 24 * time.Hour,
			ArchiveOrg:       cfg.Replication.ArchiveOrg,
			// Rescan so the inventory shows the result. Forgejo updates some
			// repository fields (e.g. "empty" after the first push) just after
			// a push, so give it a moment first. The scanner exists by then.
			AfterTriggered: func(rec store.RepositoryRecord) {
				time.AfterFunc(5*time.Second, func() { scanner.ScanRepo(ctx, rec.FullName) })
			},
		}, log)
		log.Info("replication enabled", "git", v, "work_dir", cfg.Replication.WorkDir,
			"create_missing", *cfg.Replication.CreateMissing, "auto_fix", *cfg.Replication.AutoFix,
			"hand_off_conflicts", *cfg.Replication.HandOffConflicts, "backup_days", cfg.Replication.BackupDays,
			"collaborators", cfg.Replication.Collaborators, "organizations", cfg.Replication.Organizations,
			"protect_replicas", cfg.Replication.ProtectReplicas,
			"branch_protection", cfg.Replication.BranchProtection, "metadata", cfg.Replication.Metadata, "releases", cfg.Replication.Releases, "wiki", cfg.Replication.Wiki,
			"actions", cfg.Replication.Actions, "lfs", cfg.Replication.LFS, "packages", cfg.Replication.Packages)
	}
	// Renames and primaries are assigned after scans and after webhooks;
	// one at a time.
	var assignMu sync.Mutex
	assignPrimaries := func(ctx context.Context) error {
		assignMu.Lock()
		defer assignMu.Unlock()
		return inventory.AssignPrimaries(ctx, db, nodeNames, log)
	}
	var issueSync *issues.Syncer
	if engine != nil && cfg.Replication.Issues {
		issueSync = issues.NewSyncer(issueNodes, db, monitor, issues.Options{
			Concurrency: cfg.Replication.Concurrency, EnsureUser: engine.EnsureUser,
			Reactions: cfg.Replication.Reactions, Attachments: cfg.Replication.Attachments,
			AttachmentMax: cfg.Replication.AttachmentMaxBytes,
			PullRequests:  cfg.Replication.PullRequests, Reviews: cfg.Replication.Reviews}, log)
		log.Info("issue replication enabled", "reactions", cfg.Replication.Reactions,
			"attachments", cfg.Replication.Attachments, "pull_requests", cfg.Replication.PullRequests,
			"reviews", cfg.Replication.Reviews)
	}
	detector := conflicts.NewDetector(nodeNames, comparers, db, log)
	detector.ReplicationOwnsPrimaries = engine != nil
	scanner = inventory.NewScanner(scanTargets, inventory.Options{
		Interval:          cfg.Inventory.Interval,
		BranchConcurrency: cfg.Inventory.BranchConcurrency,
		// Archived copies of deleted repositories aren't inventoried.
		SkipOwners: []string{cfg.Replication.ArchiveOrg},
		AfterScan: func(ctx context.Context) {
			if err := assignPrimaries(ctx); err != nil {
				log.Error("assigning primaries failed", "error", err)
			}
			if err := detector.Run(ctx); err != nil {
				log.Error("conflict detection failed", "error", err)
			}
			if engine != nil {
				engine.RunAll(ctx)
			}
			if issueSync != nil {
				issueSync.RunAll(ctx)
			}
		},
	}, db, log)
	var hooks http.Handler
	var hookStatus *webhook.Tracker
	var installer *webhook.Installer
	if cfg.Webhooks.URL != "" {
		hookStatus = webhook.NewTracker(nodeNames)
		installer = &webhook.Installer{
			Targets: hookTargets, BaseURL: cfg.Webhooks.URL, Secret: cfg.Webhooks.Secret,
			Interval: cfg.Webhooks.CheckInterval, Tracker: hookStatus, Log: log,
			Audit: func(ctx context.Context, action, target string, details map[string]any) {
				if err := db.Audit(ctx, "forgesync", action, target, details); err != nil {
					log.Error("writing audit log failed", "error", err)
				}
			},
		}
		dispatch := &webhook.RepoDispatcher{Store: db, Scanner: scanner, Assign: assignPrimaries, Log: log}
		if engine != nil { // a nil *Engine in the interface would look enabled
			dispatch.Replicator = engine
		}
		if issueSync != nil {
			dispatch.Issues = issueSync
		}
		// A standby doesn't act on what the nodes report: the hooks point
		// at whichever controller is leading, and the leader's own scans
		// pick up anything that arrived at the wrong one.
		hooks = &webhook.Receiver{Secret: cfg.Webhooks.Secret, ServiceUsers: serviceUsers,
			Dispatch: leaderDispatcher{dispatch, elector.Leading, log},
			Tracker:  hookStatus, Log: log, Context: ctx}
		log.Info("webhooks enabled", "url", cfg.Webhooks.URL, "events", webhook.Events)
	}
	monitorDone := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(4)
		go func() { defer wg.Done(); elector.Run(ctx) }()
		// Every controller says it's there, leader or not, so the UI can
		// show them all and say which one is acting. On the same beat as
		// the lease, so a controller that has stopped shows as stale at
		// about the same time as its lease runs out.
		go func() {
			defer wg.Done()
			recordController(ctx, db, holder, cfg, startedAt, log)
		}()
		// The health monitor runs on both controllers, so the standby's
		// pages are live too; only the leader writes what it finds (see
		// leaderRecorder).
		go func() { defer wg.Done(); monitor.Run(ctx) }()
		// Everything that changes a node or decides anything runs only
		// while this controller holds the lease, and stops the moment it
		// doesn't.
		go func() {
			defer wg.Done()
			elector.Supervise(ctx, func(lctx context.Context) {
				var lwg sync.WaitGroup
				lwg.Add(1)
				go func() { defer lwg.Done(); scanner.Run(lctx) }()
				if installer != nil {
					lwg.Add(1)
					go func() { defer lwg.Done(); installer.Run(lctx) }()
				}
				lwg.Wait()
			})
		}()
		wg.Wait()
		close(monitorDone)
	}()

	if cfg.HTTP.AdminToken == "" {
		log.Warn("no admin token: the CLI and the break-glass sign-in are disabled; " +
			"set http.admin_token_file")
	}
	// Only a non-nil engine: a nil *Engine in the interface would look enabled.
	var replicator api.Replicator
	if engine != nil {
		replicator = engine
	}
	srv := &http.Server{
		Addr: cfg.HTTP.Listen,
		Handler: (&api.Server{
			AdminToken:          cfg.HTTP.AdminToken,
			Nodes:               infos,
			Health:              monitor,
			Leader:              elector,
			Inventory:           scanner,
			Replication:         replicator,
			Users:               userProvisioner(engine),
			ControllerName:      cfg.Controller.Name,
			ControllerBeat:      cfg.Controller.Renew,
			ReplicationFeatures: replicationFeatures(cfg),
			DB:                  db,
			Log:                 log,
			StartedAt:           startedAt,
			SecureCookies:       *cfg.HTTP.SecureCookies,
			Frontend:            webui.Handler(),
			Webhooks:            hooks,
			WebhookStatus:       statusOrNil(hookStatus),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTP.Listen)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		stop()
		<-monitorDone
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	<-monitorDone
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func newLogger(c config.Log) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// leaderRecorder keeps the node history one controller's story. Both watch
// the nodes, so both can show them, but a state change is written once, by
// whichever is acting.
type leaderRecorder struct {
	rec     health.Recorder
	leading func() bool
}

func (l leaderRecorder) RecordNodeStatus(ctx context.Context, s health.Status, prev health.State) error {
	if !l.leading() {
		return nil
	}
	return l.rec.RecordNodeStatus(ctx, s, prev)
}

// leaderDispatcher drops what the nodes report unless this controller is
// the one acting.
type leaderDispatcher struct {
	to      webhook.Dispatcher
	leading func() bool
	log     *slog.Logger
}

func (d leaderDispatcher) Changed(ctx context.Context, c webhook.Change) {
	if !d.leading() {
		d.log.Debug("webhook ignored: this controller is on standby", "repository", c.Repository, "node", c.Node)
		return
	}
	d.to.Changed(ctx, c)
}

// statusOrNil keeps a nil tracker a nil interface, so the API reports
// webhooks as off.
func statusOrNil(t *webhook.Tracker) interface{ Snapshot() []webhook.Status } {
	if t == nil {
		return nil
	}
	return t
}

// replicationFeatures names what this controller keeps the same besides
// branches and tags, for the UI. It's built from the configuration rather
// than from a list in the UI, so a page can never claim something this
// controller isn't doing.
func replicationFeatures(cfg *config.Config) []string {
	r := cfg.Replication
	if !r.Enabled {
		return nil
	}
	features := []string{"branches and tags"}
	add := func(on bool, name string) {
		if on {
			features = append(features, name)
		}
	}
	add(r.LFS, "LFS objects")
	add(r.Packages, "packages (generic and maven)")
	add(r.Wiki, "the wiki")
	add(r.Releases, "releases and their files")
	add(r.Issues, "issues and comments")
	add(r.PullRequests, "pull requests")
	add(r.Reviews, "reviews")
	add(r.Reactions, "reactions")
	add(r.Attachments, "attachments")
	add(r.Metadata, "settings and topics")
	add(r.Collaborators, "collaborators")
	add(r.Organizations, "organizations and teams")
	add(r.BranchProtection, "branch protection")
	add(r.Actions, "Actions variables")
	return features
}

// userProvisioner is the engine, or nil when replication is off: without
// node tokens ForgeSync can't create an account anywhere.
func userProvisioner(engine *replication.Engine) api.UserProvisioner {
	if engine == nil {
		return nil
	}
	return userAdmin{engine}
}

// userAdmin adapts the engine's own types to the API's, so the API
// package doesn't depend on the replication package's options.
type userAdmin struct{ *replication.Engine }

func (u userAdmin) CreateUser(ctx context.Context, login, subject, fullName, email, home string) ([]string, map[string]string, error) {
	return u.Engine.CreateUser(ctx, replication.NewUser{
		Login: login, Subject: subject, FullName: fullName, Email: email, Home: home})
}

// recordController writes this controller's heartbeat until ctx ends, and
// says it has gone on the way out, so a planned stop doesn't leave a
// controller looking merely late.
func recordController(ctx context.Context, db *store.Store, holder string, cfg *config.Config, startedAt time.Time, log *slog.Logger) {
	rec := store.ControllerRecord{Name: cfg.Controller.Name, Holder: holder, URL: cfg.Controller.URL,
		Version: buildinfo.Version, Priority: cfg.Controller.Priority, StartedAt: startedAt.UTC()}
	write := func() {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := db.RecordController(wctx, rec); err != nil && ctx.Err() == nil {
			log.Warn("recording this controller failed", "error", err)
		}
	}
	write()
	beat := cfg.Controller.Renew
	if beat <= 0 {
		beat = cfg.Controller.Lease / 3
	}
	ticker := time.NewTicker(beat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			write()
		}
	}
}

// yieldToBetterController answers whether this controller should hand the
// lease over, which is how leadership ends up where someone meant it to
// be rather than wherever it happened to land.
//
// Two reasons to step aside. An administrator has chosen another
// controller (the button on its page), which beats everything else until
// it's cleared. Or, with no choice made, a controller configured to lead
// before this one is running and has said so recently.
//
// "Recently" is three heartbeats either way: handing over to a controller
// that has stopped would cost a whole lease of nobody doing anything, so
// this waits to see it alive first. With no priority and no choice (the
// defaults) nobody yields and whoever holds the lease keeps it.
func yieldToBetterController(db *store.Store, cfg *config.Config, log *slog.Logger) func(context.Context) bool {
	mine := cfg.Controller.Priority
	beat := cfg.Controller.Renew
	if beat <= 0 {
		beat = cfg.Controller.Lease / 3
	}
	return func(ctx context.Context) bool {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		chosen, err := db.Chosen(cctx)
		if err != nil {
			log.Warn("leadership: couldn't read the chosen controller; staying as we are", "error", err)
			return false
		}
		others, err := db.Controllers(cctx)
		if err != nil {
			log.Warn("leadership: couldn't read the other controllers; staying as we are", "error", err)
			return false
		}
		alive := func(c store.ControllerRecord) bool { return time.Since(c.LastSeenAt) <= 3*beat }
		if chosen.Controller != "" {
			if chosen.Controller == cfg.Controller.Name {
				return false // we are the one they asked for
			}
			for _, c := range others {
				if c.Name == chosen.Controller && alive(c) {
					log.Info("leadership: an administrator chose another controller", "controller", c.Name,
						"chosen_by", chosen.ChosenBy)
					return true
				}
			}
			return false // chosen but not running: keep working
		}
		if mine == 0 {
			return false
		}
		for _, c := range others {
			if c.Name == cfg.Controller.Name || c.Priority == 0 || c.Priority >= mine {
				continue
			}
			if alive(c) {
				log.Info("leadership: a controller that should lead is running", "controller", c.Name,
					"priority", c.Priority, "ours", mine)
				return true
			}
		}
		return false
	}
}
