package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/health"
)

// Metrics are for whatever watches the installation: the numbers a person
// would otherwise have to read off the dashboard. They're in the
// Prometheus text format, which is what most things speak, and they're
// gathered when asked rather than kept, so nothing has to be kept in step.
//
// What matters here is the difference between "ForgeSync is running" and
// "ForgeSync is doing its job": a controller can be up, reachable and
// entirely idle because it's on standby, because the database is gone or
// because every node is unreachable. So the series say which controller is
// acting, when each node was last seen, when each was last scanned, and
// how many replicas are in each state -- an alert on any of those catches
// a sync that has quietly stopped, which uptime alone never would.
//
// The endpoint needs the Viewer role like every other read, so a scraper
// uses the admin token as its bearer token.

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var b strings.Builder
	now := time.Now()

	role, leaderInfo := s.leadership()
	write(&b, "forgesync_build_info", "the running version", "gauge",
		sample{labels: map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit}, value: 1})
	write(&b, "forgesync_uptime_seconds", "how long this controller has been running", "gauge",
		sample{value: now.Sub(s.StartedAt).Seconds()})

	// Leadership: 1 on the controller that is doing the work. A single
	// controller is always the one.
	acting := 0.0
	if role == "single" || role == "leader" {
		acting = 1
	}
	name := ""
	if leaderInfo != nil {
		name = leaderInfo.Name
	}
	write(&b, "forgesync_leader", "1 on the controller that is acting, 0 on a standby", "gauge",
		sample{labels: map[string]string{"role": role, "leader": name}, value: acting})

	up := 1.0
	if err := s.DB.Ping(ctx); err != nil {
		up = 0
	}
	write(&b, "forgesync_database_up", "1 when the database answers", "gauge", sample{value: up})

	// Nodes: one series per node, so an alert can name the node that went.
	var healthy, nodes []sample
	var lastSeen, failures []sample
	for _, n := range s.nodes() {
		labels := map[string]string{"node": n.Node}
		ok := 0.0
		if n.State == health.Healthy {
			ok = 1
		}
		healthy = append(healthy, sample{labels: labels, value: ok})
		failures = append(failures, sample{labels: labels, value: float64(n.ConsecutiveFailures)})
		if n.LastSeen != nil {
			lastSeen = append(lastSeen, sample{labels: labels, value: float64(n.LastSeen.Unix())})
		}
		nodes = append(nodes, sample{labels: map[string]string{"node": n.Node, "state": string(n.State)}, value: 1})
	}
	write(&b, "forgesync_node_healthy", "1 when the node answers and its token works", "gauge", healthy...)
	write(&b, "forgesync_node_state", "the node's current state, as a label", "gauge", nodes...)
	write(&b, "forgesync_node_consecutive_failures", "failed checks in a row", "gauge", failures...)
	write(&b, "forgesync_node_last_seen_timestamp_seconds", "when the node last answered", "gauge", lastSeen...)

	// Scans: when each node was last read. A scan that stops is a sync
	// that stops, and nothing else here would show it.
	if scans, err := s.DB.NodeScans(ctx); err == nil {
		var last, repos []sample
		for _, sc := range scans {
			labels := map[string]string{"node": sc.Node}
			if sc.LastSuccessAt != nil {
				last = append(last, sample{labels: labels, value: float64(sc.LastSuccessAt.Unix())})
			}
			repos = append(repos, sample{labels: labels, value: float64(sc.Repositories)})
		}
		write(&b, "forgesync_scan_last_success_timestamp_seconds", "when the node was last scanned through", "gauge", last...)
		write(&b, "forgesync_node_repositories", "repositories found on the node at the last scan", "gauge", repos...)
	} else {
		s.Log.Warn("metrics: reading the scans failed", "error", err)
	}

	if n, err := s.DB.OpenConflicts(ctx); err == nil {
		write(&b, "forgesync_conflicts_open", "differences waiting for a person", "gauge", sample{value: float64(n)})
	}
	if recs, err := s.DB.Repositories(ctx); err == nil {
		known, withPrimary := 0, 0
		for _, rec := range recs {
			if rec.DeletedAt != nil {
				continue
			}
			known++
			if rec.PrimaryNode != "" {
				withPrimary++
			}
		}
		write(&b, "forgesync_repositories", "repositories ForgeSync knows about", "gauge", sample{value: float64(known)})
		write(&b, "forgesync_repositories_with_primary", "repositories that have a primary node, so they replicate", "gauge",
			sample{value: float64(withPrimary)})
	}
	if s.Replication != nil {
		if counts, err := s.DB.ReplicationCounts(ctx); err == nil {
			var states []sample
			for _, state := range sortedKeys(counts) {
				states = append(states, sample{labels: map[string]string{"state": state}, value: float64(counts[state])})
			}
			write(&b, "forgesync_replicas", "copies of a repository on another node, by state", "gauge", states...)
		}
	}
	if s.WebhookStatus != nil {
		var installed, deliveries, rejected []sample
		for _, st := range s.WebhookStatus.Snapshot() {
			labels := map[string]string{"node": st.Node}
			on := 0.0
			if st.Installed {
				on = 1
			}
			installed = append(installed, sample{labels: labels, value: on})
			deliveries = append(deliveries, sample{labels: labels, value: float64(st.Deliveries)})
			rejected = append(rejected, sample{labels: labels, value: float64(st.Rejected)})
		}
		write(&b, "forgesync_webhook_installed", "1 when ForgeSync's webhook is on the node", "gauge", installed...)
		write(&b, "forgesync_webhook_deliveries_total", "webhook deliveries accepted from the node", "counter", deliveries...)
		write(&b, "forgesync_webhook_rejected_total", "webhook deliveries refused (a bad signature, usually)", "counter", rejected...)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	io.WriteString(w, b.String())
}

// sample is one series: its labels and its value.
type sample struct {
	labels map[string]string
	value  float64
}

// write prints one metric with its help and type. A metric with no
// samples is left out entirely rather than printed empty.
func write(b *strings.Builder, name, help, typ string, samples ...sample) {
	if len(samples) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	for _, s := range samples {
		fmt.Fprintf(b, "%s%s %s\n", name, labelsOf(s.labels), number(s.value))
	}
}

func labelsOf(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		parts = append(parts, k+`="`+escapeLabel(labels[k])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel follows the text format: backslash, quote and newline.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// number prints a value the way the text format wants it: no exponent for
// the whole numbers everything here actually is.
func number(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
