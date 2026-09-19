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
	"scenegit.org/forgesync/internal/auth"
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
	elector := leader.New(db, leader.Options{
		Holder: leader.NewHolderID(), Name: cfg.Controller.Name, URL: cfg.Controller.URL,
		TTL: cfg.Controller.Lease, Renew: cfg.Controller.Renew,
	}, log)
	log.Info("controller", "name", cfg.Controller.Name, "url", cfg.Controller.URL,
		"lease", cfg.Controller.Lease, "renew", cfg.Controller.Renew)

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
			API: client, SceneIDSourceID: n.SceneIDSourceID})
	}
	if err := db.SyncNodes(ctx, records); err != nil {
		return err
	}

	monitor := health.NewMonitor(targets, health.Options{
		Interval:         cfg.Health.Interval,
		Timeout:          cfg.Health.Timeout,
		FailureThreshold: cfg.Health.FailureThreshold,
	}, leaderRecorder{db, elector.Leading}, log)
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
			"branch_protection", cfg.Replication.BranchProtection, "metadata", cfg.Replication.Metadata)
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
		wg.Add(3)
		go func() { defer wg.Done(); elector.Run(ctx) }()
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

	var oidcFlow api.OIDCFlow
	if cfg.OIDC.Enabled() {
		oidcFlow = auth.NewOIDC(auth.OIDCConfig{
			Issuer:                cfg.OIDC.Issuer,
			ClientID:              cfg.OIDC.ClientID,
			ClientSecret:          cfg.OIDC.ClientSecret,
			RedirectURL:           cfg.OIDC.RedirectURL,
			PostLogoutRedirectURL: cfg.OIDC.PostLogoutRedirectURL,
			Scopes:                cfg.OIDC.Scopes,
			Roles: auth.RoleMapping{
				Claim:         cfg.OIDC.RolesClaim,
				Administrator: cfg.OIDC.Roles.Administrator,
				Operator:      cfg.OIDC.Roles.Operator,
				Viewer:        cfg.OIDC.Roles.Viewer,
			},
		})
		log.Info("SceneID sign-in enabled", "issuer", cfg.OIDC.Issuer, "token_sign_in", cfg.OIDC.AllowTokenSignIn)
	}
	if cfg.HTTP.AdminToken == "" && oidcFlow == nil {
		log.Warn("admin API disabled: set http.admin_token_file or oidc")
	}
	// Only a non-nil engine: a nil *Engine in the interface would look enabled.
	var replicator api.Replicator
	if engine != nil {
		replicator = engine
	}
	srv := &http.Server{
		Addr: cfg.HTTP.Listen,
		Handler: (&api.Server{
			AdminToken:       cfg.HTTP.AdminToken,
			OIDC:             oidcFlow,
			AllowTokenSignIn: cfg.OIDC.AllowTokenSignIn,
			Nodes:            infos,
			Health:           monitor,
			Leader:           elector,
			Inventory:        scanner,
			Replication:      replicator,
			DB:               db,
			Log:              log,
			StartedAt:        startedAt,
			SecureCookies:    *cfg.HTTP.SecureCookies,
			Frontend:         webui.Handler(),
			Webhooks:         hooks,
			WebhookStatus:    statusOrNil(hookStatus),
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
