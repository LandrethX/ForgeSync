// Package api serves the controller's HTTP interface:
//
//	/healthz   liveness, always 200 while the process runs
//	/readyz    readiness, 200 only when the database answers
//	/api/v1/*  admin API: bearer token (CLI) or session cookie (web UI, signed
//	           in with SceneID, or the admin token when SceneID is off)
//	/*         the embedded web UI
//
// The controller-to-controller (/internal/v1) and agent (/agent/v1) APIs from
// the architecture document are not part of this yet.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
	"scenegit.org/forgesync/internal/webhook"
)

// DB is the database access the API needs.
type DB interface {
	Ping(ctx context.Context) error
	Transitions(ctx context.Context, node string, limit int) ([]store.Transition, error)
	Repositories(ctx context.Context) ([]store.RepositoryRecord, error)
	Repository(ctx context.Context, id string) (store.RepositoryRecord, error)
	SetPrimary(ctx context.Context, id, node string) (string, error)
	Users(ctx context.Context) ([]store.UserRecord, error)
	User(ctx context.Context, id string) (store.UserRecord, error)
	SetUserHome(ctx context.Context, id, node string) (string, error)
	Archives(ctx context.Context, repositoryID string) ([]store.Archive, error)
	Issues(ctx context.Context, repositoryID string) ([]store.IssueRecord, []store.CommentRecord, error)
	NodeScans(ctx context.Context) ([]store.NodeScan, error)
	Conflicts(ctx context.Context, f store.ConflictFilter) ([]store.Conflict, int, map[string]int, error)
	ConflictByID(ctx context.Context, id int64) (store.Conflict, error)
	AcknowledgeConflict(ctx context.Context, id int64, actor, note string, at time.Time) error
	OpenConflicts(ctx context.Context) (int, error)
	ReplicaSyncs(ctx context.Context, repositoryID string) ([]store.ReplicaSync, error)
	ReplicationCounts(ctx context.Context) (map[string]int, error)
	History(ctx context.Context, f store.EventFilter) ([]store.Event, string, error)
	HistoryEach(ctx context.Context, f store.EventFilter, max int, fn func(store.Event) error) error
	HistoryActors(ctx context.Context) ([]string, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// HealthSource is the node monitor.
type HealthSource interface {
	Snapshot() []health.Status
	Subscribe() (<-chan struct{}, func())
}

// NodeInfo is the static part of a node, from the config.
type NodeInfo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Site string `json:"site"`
}

// Node is what /api/v1/nodes returns for each node.
type Node struct {
	NodeInfo
	health.Status
}

type Server struct {
	AdminToken string // bearer token for the CLI; also web sign-in when OIDC is off
	OIDC       OIDCFlow
	// AllowTokenSignIn keeps admin-token sign-in in the web UI while OIDC is on.
	AllowTokenSignIn bool
	Nodes            []NodeInfo
	Health           HealthSource
	Inventory        Inventory
	Replication      Replicator // nil when replication is off
	DB               DB
	Log              *slog.Logger
	StartedAt        time.Time
	Sessions         *Sessions
	SecureCookies    bool
	Frontend         http.Handler // nil serves nothing outside the API
	// Webhooks receives Forgejo's deliveries at /api/v1/hooks/forgejo/{node};
	// it checks their signatures itself. nil when webhooks are off.
	Webhooks http.Handler
	// WebhookStatus reports each node's webhook; nil when webhooks are off.
	WebhookStatus interface{ Snapshot() []webhook.Status }

	limiter *loginLimiter
}

func (s *Server) Handler() http.Handler {
	if s.Sessions == nil {
		s.Sessions = NewSessions(8*time.Hour, 30*time.Minute)
	}
	s.limiter = newLoginLimiter(10, 5*time.Minute)

	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.logRequests, middleware.Recoverer, securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.readyz)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(noStore)
		r.Get("/auth/config", s.authConfig)
		r.Get("/auth/login", s.oidcLogin)
		r.Get("/auth/callback", s.oidcCallback)
		r.Post("/session", s.createSession)
		if s.Webhooks != nil {
			// Signed by the node, not signed in: no session or token.
			r.Post("/hooks/forgejo/{node}", func(w http.ResponseWriter, r *http.Request) {
				r.SetPathValue("node", chi.URLParam(r, "node"))
				s.Webhooks.ServeHTTP(w, r)
			})
		}
		r.Get("/session", s.getSession)
		r.Delete("/session", s.deleteSession)

		r.Group(func(r chi.Router) {
			r.Use(s.authenticate)
			r.Group(func(r chi.Router) {
				r.Use(requireRole(auth.Viewer))
				r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, http.StatusOK, map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit})
				})
				r.Get("/overview", s.overview)
				r.Get("/nodes", s.listNodes)
				r.Get("/nodes/{name}", s.getNode)
				r.Get("/nodes/{name}/transitions", s.nodeTransitions)
				r.Get("/transitions", s.nodeTransitions)
				r.Get("/events", s.events)
				r.Get("/repositories", s.listRepositories)
				r.Get("/repositories/{id}", s.getRepository)
				r.Get("/repositories/{id}/issues", s.repositoryIssues)
				r.Get("/inventory", s.inventoryStatus)
				r.Get("/users", s.listUsers)
				r.Get("/webhooks", s.webhookStatus)
				r.Get("/users/{id}", s.getUser)
				r.Get("/conflicts", s.listConflicts)
				r.Get("/conflicts/{id}", s.getConflict)
			})
			r.With(requireRole(auth.Operator)).Post("/conflicts/{id}/acknowledge", s.acknowledgeConflict)
			r.With(requireRole(auth.Operator)).Post("/inventory/scan", s.scanNow)
			r.With(requireRole(auth.Administrator)).Put("/repositories/{id}/primary", s.setPrimary)
			r.With(requireRole(auth.Administrator)).Put("/users/{id}/home", s.setUserHome)
			r.With(requireRole(auth.Operator)).Post("/repositories/{id}/replicate", s.replicateNow)
			// The history shows who signed in from where: operators and up.
			r.Group(func(r chi.Router) {
				r.Use(requireRole(auth.Operator))
				r.Get("/history", s.listHistory)
				r.Get("/history/actors", s.historyActors)
				r.Get("/history/export", s.exportHistory)
			})
		})
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		})
	})

	if s.Frontend != nil {
		r.NotFound(s.Frontend.ServeHTTP)
	}
	return r
}

// ---------------------------------------------------------------- data

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) nodes() []Node {
	status := map[string]health.Status{}
	for _, st := range s.Health.Snapshot() {
		status[st.Node] = st
	}
	out := make([]Node, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		st, ok := status[n.Name]
		if !ok {
			st = health.Status{Node: n.Name, State: health.Unknown}
		}
		out = append(out, Node{NodeInfo: n, Status: st})
	}
	return out
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.nodes())
}

func (s *Server) getNode(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	for _, n := range s.nodes() {
		if n.Name == name {
			writeJSON(w, http.StatusOK, n)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "no node named " + name})
}

func (s *Server) nodeTransitions(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 50, 1, 500)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	tr, err := s.DB.Transitions(r.Context(), chi.URLParam(r, "name"), limit)
	if err != nil {
		s.serverError(w, "list transitions", err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

// Overview is the dashboard summary.
type Overview struct {
	Version   string         `json:"version"`
	Commit    string         `json:"commit"`
	StartedAt time.Time      `json:"started_at"`
	Role      string         `json:"role"`
	Database  DatabaseStatus `json:"database"`
	Nodes     map[string]int `json:"nodes"` // count per state, plus "total"
	// OpenConflicts is -1 when it couldn't be counted.
	OpenConflicts int                `json:"open_conflicts"`
	Replication   ReplicationSummary `json:"replication"`
}

// ReplicationSummary counts replicas (of repositories with a primary) by state.
type ReplicationSummary struct {
	Enabled bool           `json:"enabled"`
	Counts  map[string]int `json:"counts"`
}

type DatabaseStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	o := Overview{
		Version:   buildinfo.Version,
		Commit:    buildinfo.Commit,
		StartedAt: s.StartedAt.UTC(),
		// Leader election arrives with the HA phase; until then there is one controller.
		Role:     "single",
		Database: DatabaseStatus{OK: true},
		Nodes:    map[string]int{"total": 0},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		o.Database = DatabaseStatus{OK: false, Error: "unreachable"}
		s.Log.Warn("database ping failed", "error", err)
	}
	if n, err := s.DB.OpenConflicts(ctx); err != nil {
		o.OpenConflicts = -1
		s.Log.Warn("counting conflicts failed", "error", err)
	} else {
		o.OpenConflicts = n
	}
	o.Replication = ReplicationSummary{Enabled: s.Replication != nil, Counts: map[string]int{}}
	if s.Replication != nil {
		if counts, err := s.DB.ReplicationCounts(ctx); err != nil {
			s.Log.Warn("counting replication states failed", "error", err)
		} else {
			o.Replication.Counts = counts
		}
	}
	for _, n := range s.nodes() {
		o.Nodes[string(n.State)]++
		o.Nodes["total"]++
	}
	writeJSON(w, http.StatusOK, o)
}

// events streams the node list as server-sent events after every health
// check, with a comment heartbeat to keep proxies from closing the stream.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// The server's WriteTimeout would cut the stream; lift it for this response.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.serverError(w, "event stream", err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	changes, unsubscribe := s.Health.Subscribe()
	defer unsubscribe()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	send := func() bool {
		b, err := json.Marshal(s.nodes())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: nodes\ndata: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if _, err := fmt.Fprint(w, "retry: 5000\n\n"); err != nil || !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			if !s.stillSignedIn(r) {
				fmt.Fprint(w, "event: signed-out\ndata: {}\n\n")
				rc.Flush()
				return
			}
			if !send() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// ---------------------------------------------------------------- helpers

func queryInt(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be a number from %d to %d", name, min, max)
	}
	return n, nil
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what+" failed", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"message": what + " failed"})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.Log.Debug("http request",
			"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
			"duration", time.Since(start), "request_id", middleware.GetReqID(r.Context()))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
