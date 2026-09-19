package webhook

import (
	"context"
	"log/slog"
	neturl "net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
)

// HookAPI is the part of the Forgejo admin API the installer needs.
type HookAPI interface {
	SystemHooks(ctx context.Context) ([]forgejo.Hook, error)
	CreateSystemHook(ctx context.Context, url, secret string, events []string) (forgejo.Hook, error)
	DeleteSystemHook(ctx context.Context, id int64) error
}

// Target is a node to install the webhook on.
type Target struct {
	Name   string
	Client HookAPI
}

// Installer keeps ForgeSync's system webhook in place on every node.
type Installer struct {
	Targets  []Target
	BaseURL  string // e.g. https://sync.scenegit.org/api/v1/hooks/forgejo
	Secret   string // the configured secret; each node gets NodeSecret(Secret, node)
	Interval time.Duration
	Tracker  *Tracker
	Log      *slog.Logger
	// Audit, if set, records installs and replacements.
	Audit func(ctx context.Context, action, target string, details map[string]any)
	now   func() time.Time
}

// Run checks every node now and then on every interval, until ctx ends.
func (in *Installer) Run(ctx context.Context) {
	for {
		in.EnsureAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(in.Interval):
		}
	}
}

// EnsureAll checks every node in parallel.
func (in *Installer) EnsureAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range in.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in.Ensure(ctx, t)
		}()
	}
	wg.Wait()
}

// Ensure makes the node have exactly one ForgeSync webhook with the current
// URL, events and secret: it keeps a matching one, removes stale ones (an
// older secret or URL) and creates it if missing.
func (in *Installer) Ensure(ctx context.Context, t Target) {
	now := time.Now
	if in.now != nil {
		now = in.now
	}
	secret := NodeSecret(in.Secret, t.Name)
	want := HookURL(in.BaseURL, t.Name, secret)
	fail := func(err error) {
		in.Log.Warn("checking the ForgeSync webhook failed", "node", t.Name, "error", err)
		at := now().UTC()
		in.Tracker.update(t.Name, func(s *Status) { s.Error, s.CheckedAt = err.Error(), &at })
	}

	hooks, err := t.Client.SystemHooks(ctx)
	if err != nil {
		fail(err)
		return
	}
	var keep int64
	for _, h := range hooks {
		url := h.Config["url"]
		if url == "" {
			url = h.URL
		}
		if !isOurs(in.BaseURL, t.Name, url) {
			continue // someone else's hook
		}
		if keep == 0 && url == want && h.Active && hasEvents(h.Events, Events) {
			keep = h.ID
			continue
		}
		if err := t.Client.DeleteSystemHook(ctx, h.ID); err != nil {
			fail(err)
			return
		}
		in.Log.Info("removed a stale ForgeSync webhook", "node", t.Name, "hook", h.ID)
	}
	if keep == 0 {
		h, err := t.Client.CreateSystemHook(ctx, want, secret, Events)
		if err != nil {
			fail(err)
			return
		}
		keep = h.ID
		in.Log.Info("installed the ForgeSync webhook", "node", t.Name, "hook", h.ID)
		if in.Audit != nil {
			in.Audit(ctx, "node.webhook_installed", t.Name, map[string]any{"hook_id": h.ID, "events": Events})
		}
	}
	at := now().UTC()
	in.Tracker.update(t.Name, func(s *Status) { s.Installed, s.HookID, s.Error, s.CheckedAt = true, keep, "", &at })
}

// isOurs reports that a hook on the node is ForgeSync's own, whichever
// controller installed it. The host is whoever was leading at the time, so
// only the path counts: after a failover the new leader replaces the old
// one's hook instead of adding a second.
func isOurs(base, node, hookURL string) bool {
	b, err := neturl.Parse(base)
	if err != nil {
		return false
	}
	u, err := neturl.Parse(hookURL)
	if err != nil {
		return false
	}
	return u.Path == strings.TrimRight(b.Path, "/")+"/"+node
}

// hasEvents reports whether a hook gets every event ForgeSync needs.
// Forgejo lists some events expanded ("issues" also comes back as
// issue_assign, issue_label and issue_milestone), so extra ones are fine.
func hasEvents(got, need []string) bool {
	for _, x := range need {
		if !slices.Contains(got, x) {
			return false
		}
	}
	return true
}
