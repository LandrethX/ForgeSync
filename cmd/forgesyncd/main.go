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
	"syscall"
	"time"

	"scenegit.org/forgesync/internal/api"
	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/config"
	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
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
	for _, n := range cfg.Nodes {
		client, err := forgejo.New(n.URL, n.Token, nil)
		if err != nil {
			return fmt.Errorf("node %s: %w", n.Name, err)
		}
		records = append(records, store.NodeRecord{Name: n.Name, URL: n.URL, Site: n.Site})
		infos = append(infos, api.NodeInfo{Name: n.Name, URL: n.URL, Site: n.Site})
		targets = append(targets, health.Target{Name: n.Name, ServiceUser: n.ServiceUser, Client: client})
	}
	if err := db.SyncNodes(ctx, records); err != nil {
		return err
	}

	monitor := health.NewMonitor(targets, health.Options{
		Interval:         cfg.Health.Interval,
		Timeout:          cfg.Health.Timeout,
		FailureThreshold: cfg.Health.FailureThreshold,
	}, db, log)
	monitorDone := make(chan struct{})
	go func() {
		monitor.Run(ctx)
		close(monitorDone)
	}()

	if cfg.HTTP.AdminToken == "" {
		log.Warn("admin API disabled: http.admin_token_file is not set")
	}
	srv := &http.Server{
		Addr: cfg.HTTP.Listen,
		Handler: (&api.Server{
			AdminToken:    cfg.HTTP.AdminToken,
			Nodes:         infos,
			Health:        monitor,
			DB:            db,
			Log:           log,
			StartedAt:     startedAt,
			SecureCookies: *cfg.HTTP.SecureCookies,
			Frontend:      webui.Handler(),
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
