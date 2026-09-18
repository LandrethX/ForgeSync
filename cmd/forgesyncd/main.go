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
	"scenegit.org/forgesync/internal/replication"
	"scenegit.org/forgesync/internal/store"
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

	var records []store.NodeRecord
	var infos []api.NodeInfo
	var targets []health.Target
	var scanTargets []inventory.Target
	var nodeNames []string
	comparers := map[string]conflicts.Comparer{}
	var gitNodes []replication.Node
	for _, n := range cfg.Nodes {
		client, err := forgejo.New(n.URL, n.Token, nil)
		if err != nil {
			return fmt.Errorf("node %s: %w", n.Name, err)
		}
		records = append(records, store.NodeRecord{Name: n.Name, URL: n.URL, Site: n.Site})
		infos = append(infos, api.NodeInfo{Name: n.Name, URL: n.URL, Site: n.Site})
		targets = append(targets, health.Target{Name: n.Name, ServiceUser: n.ServiceUser, Client: client})
		scanTargets = append(scanTargets, inventory.Target{Name: n.Name, Client: client})
		nodeNames = append(nodeNames, n.Name)
		comparers[n.Name] = client
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
	}, db, log)
	var scanner *inventory.Scanner
	var engine *replication.Engine
	if cfg.Replication.Enabled {
		git := &replication.Git{Bin: cfg.Replication.Git, WorkDir: cfg.Replication.WorkDir}
		v, err := git.Version(ctx)
		if err != nil {
			return fmt.Errorf("replication is enabled but %w", err)
		}
		engine = replication.NewEngine(gitNodes, git, db, monitor, replication.Options{
			Concurrency:   cfg.Replication.Concurrency,
			CreateMissing: *cfg.Replication.CreateMissing,
			// Rescan so the inventory shows the result. Forgejo updates some
			// repository fields (e.g. "empty" after the first push) just after
			// a push, so give it a moment first. The scanner exists by then.
			AfterTriggered: func() { time.AfterFunc(5*time.Second, func() { scanner.Trigger() }) },
		}, log)
		log.Info("replication enabled", "git", v, "work_dir", cfg.Replication.WorkDir,
			"create_missing", *cfg.Replication.CreateMissing)
	}
	detector := conflicts.NewDetector(nodeNames, comparers, db, log)
	detector.ReplicationOwnsPrimaries = engine != nil
	scanner = inventory.NewScanner(scanTargets, inventory.Options{
		Interval:          cfg.Inventory.Interval,
		BranchConcurrency: cfg.Inventory.BranchConcurrency,
		AfterScan: func(ctx context.Context) {
			if err := inventory.AssignOrigins(ctx, db, nodeNames, log); err != nil {
				log.Error("assigning primaries failed", "error", err)
			}
			if err := detector.Run(ctx); err != nil {
				log.Error("conflict detection failed", "error", err)
			}
			if engine != nil {
				engine.RunAll(ctx)
			}
		},
	}, db, log)
	monitorDone := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); monitor.Run(ctx) }()
		go func() { defer wg.Done(); scanner.Run(ctx) }()
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
			Inventory:        scanner,
			Replication:      replicator,
			DB:               db,
			Log:              log,
			StartedAt:        startedAt,
			SecureCookies:    *cfg.HTTP.SecureCookies,
			Frontend:         webui.Handler(),
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
