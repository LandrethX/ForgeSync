// Package api serves the controller's HTTP interface:
//
//	/healthz   liveness, always 200 while the process runs
//	/readyz    readiness, 200 only when the database answers
//	/api/v1/*  admin API: bearer token (CLI) or session cookie (web UI, signed
//	           in with a ForgeSync account, or the admin token as break-glass)
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
	"net/netip"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/leader"
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
	DismissConflict(ctx context.Context, id int64, actor, note string, at time.Time) error
	ReopenConflict(ctx context.Context, id int64) error
	OpenConflicts(ctx context.Context) (int, error)
	ReplicaSyncs(ctx context.Context, repositoryID string) ([]store.ReplicaSync, error)
	ReplicationCounts(ctx context.Context) (map[string]int, error)
	SourcePairs(ctx context.Context) ([]store.SourcePair, error)
	Controllers(ctx context.Context) ([]store.ControllerRecord, error)
	Chosen(ctx context.Context) (store.LeadershipChoice, error)
	CreateSession(ctx context.Context, hash string, identity []byte, expires time.Time, idle time.Duration) error
	Session(ctx context.Context, hash string, idle time.Duration, touch bool) ([]byte, time.Time, bool, error)
	DeleteSession(ctx context.Context, hash string) error
	Accounts(ctx context.Context) ([]store.Account, error)
	Account(ctx context.Context, id string) (store.Account, error)
	CreateAccount(ctx context.Context, username, password, fullName, role, by string) (store.Account, error)
	UpdateAccount(ctx context.Context, id, fullName, role string, disabled bool) (store.Account, error)
	SetAccountPassword(ctx context.Context, id, password string) error
	DeleteAccount(ctx context.Context, id string) error
	CheckPassword(ctx context.Context, username, password string) (store.Account, bool, error)
	Choose(ctx context.Context, controller, by string) error
	ClearChoice(ctx context.Context) error
	History(ctx context.Context, f store.EventFilter) ([]store.Event, string, error)
	HistoryEach(ctx context.Context, f store.EventFilter, max int, fn func(store.Event) error) error
	HistoryActors(ctx context.Context) ([]string, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// Leadership is the controller's side of the leader election. It is nil
// when ForgeSync runs as a single controller.
type Leadership interface {
	State() leader.State
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

// Server is the controller's HTTP interface. Every field is set by the
// caller; Handler builds the router from them, and what is nil is a
// feature this controller does not have (no replication, no webhooks).
type Server struct {
	// AdminToken is the bearer token for the CLI, and the break-glass
	// sign-in for the web UI: it's how the first ForgeSync account gets
	// made. People sign in with ForgeSync accounts otherwise.
	AdminToken  string
	Nodes       []NodeInfo
	Health      HealthSource
	Leader      Leadership
	Inventory   Inventory
	Replication Replicator // nil when replication is off
	// ControllerName is this controller's own name, so the dashboard can
	// mark it among the others. ControllerBeat is how often each writes
	// its heartbeat, which says when one has been quiet too long.
	ControllerName string
	ControllerBeat time.Duration
	// Users creates accounts on the nodes for Administrators; nil when
	// replication is off, since then ForgeSync has no node tokens.
	Users UserProvisioner
	// ReplicationFeatures names what replication covers besides branches
	// and tags ("issues", "releases", ...), in reading order.
	ReplicationFeatures []string
	DB                  DB
	Log                 *slog.Logger
	StartedAt           time.Time
	Sessions            *Sessions
	SecureCookies       bool
	// TrustedProxies are the proxies whose X-Forwarded-For is believed,
	// from http.trusted_proxies. Empty means the connection's own address
	// is what the history records and the sign-in limiter counts.
	TrustedProxies []netip.Prefix
	Frontend       http.Handler // nil serves nothing outside the API
	// Webhooks receives Forgejo's deliveries at /api/v1/hooks/forgejo/{node};
	// it checks their signatures itself. nil when webhooks are off.
	Webhooks http.Handler
	// WebhookStatus reports each node's webhook; nil when webhooks are off.
	WebhookStatus interface{ Snapshot() []webhook.Status }

	limiter *loginLimiter
}

// Handler builds the router: the probes, the metrics, the admin API and
// the embedded UI, with authentication, roles and the leader check around
// the parts that need them.
func (s *Server) Handler() http.Handler {
	if s.Sessions == nil {
		s.Sessions = NewSessions(8*time.Hour, 30*time.Minute, s.DB, s.Log)
	}
	s.limiter = newLoginLimiter(10, 5*time.Minute)

	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.logRequests, middleware.Recoverer, securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.readyz)
	// Metrics are a read like any other: Viewer, which the admin token is,
	// so a scraper sends it as a bearer token.
	r.Group(func(r chi.Router) {
		r.Use(noStore, s.authenticate, requireRole(auth.Viewer))
		r.Get("/metrics", s.metrics)
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(noStore)
		r.Get("/auth/config", s.authConfig)
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
				r.Get("/replication/sources", s.replicationSources)
				r.Get("/users", s.listUsers)
				r.Get("/webhooks", s.webhookStatus)
				r.Get("/users/{id}", s.getUser)
				r.Get("/conflicts", s.listConflicts)
				r.Get("/conflicts/{id}", s.getConflict)
			})
			// Choosing which controller leads is asked of whichever one a
			// person is looking at -- usually the standby, since that's
			// where someone goes to promote it -- so it isn't behind
			// requireLeader. It writes the choice and nothing else; the
			// leader reads it and steps aside of its own accord.
			// What someone writes about a conflict -- a note, a dismissal --
			// is ForgeSync's own bookkeeping in the shared database and
			// changes nothing on a node, so it works on either controller.
			r.Group(func(r chi.Router) {
				r.Use(requireRole(auth.Operator))
				r.Post("/conflicts/{id}/acknowledge", s.acknowledgeConflict)
				r.Post("/conflicts/{id}/dismiss", s.dismissConflict)
				r.Post("/conflicts/{id}/reopen", s.reopenConflict)
			})
			r.Group(func(r chi.Router) {
				r.Use(requireRole(auth.Administrator))
				r.Put("/leadership", s.chooseLeader)
				r.Delete("/leadership", s.clearLeaderChoice)
				// ForgeSync's own accounts live in the shared database and
				// belong to neither controller, so these aren't behind
				// requireLeader either: someone locked out of one
				// controller can still put it right from the other.
				r.Get("/accounts", s.listAccounts)
				r.Post("/accounts", s.createAccount)
				r.Put("/accounts/{id}", s.updateAccount)
				r.Put("/accounts/{id}/password", s.setAccountPassword)
				r.Delete("/accounts/{id}", s.deleteAccount)
			})
			// Writes belong to the controller that's acting; see requireLeader.
			r.Group(func(r chi.Router) {
				r.Use(s.requireLeader)
				r.With(requireRole(auth.Operator)).Post("/inventory/scan", s.scanNow)
				r.With(requireRole(auth.Administrator)).Put("/repositories/{id}/primary", s.setPrimary)
				r.With(requireRole(auth.Administrator)).Put("/users/{id}/home", s.setUserHome)
				r.With(requireRole(auth.Administrator)).Post("/users", s.createUser)
				r.With(requireRole(auth.Administrator)).Post("/users/{id}/nodes", s.provisionUser)
				r.With(requireRole(auth.Operator)).Post("/repositories/{id}/replicate", s.replicateNow)
			})
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
	Version   string    `json:"version"`
	Commit    string    `json:"commit"`
	StartedAt time.Time `json:"started_at"`
	// Role is single, leader or standby.
	Role     string         `json:"role"`
	Leader   *LeaderInfo    `json:"leader,omitempty"`
	Database DatabaseStatus `json:"database"`
	Nodes    map[string]int `json:"nodes"` // count per state, plus "total"
	// OpenConflicts is -1 when it couldn't be counted.
	OpenConflicts int                `json:"open_conflicts"`
	Replication   ReplicationSummary `json:"replication"`
	// Controllers is every controller sharing this database, with the one
	// that is acting marked. It's empty when the database can't be read.
	Controllers []ControllerInfo `json:"controllers"`
	// Chosen is the controller an administrator asked to lead, if any.
	Chosen *store.LeadershipChoice `json:"chosen,omitempty"`
}

// ControllerInfo is one ForgeSync controller, for the dashboard: where it
// is, what it's doing, and when it was last heard from.
type ControllerInfo struct {
	Name    string `json:"name"`
	URL     string `json:"url,omitempty"`
	Address string `json:"address,omitempty"`
	Version string `json:"version,omitempty"`
	// Role is "leader" (doing the work), "standby" (ready to take over),
	// or "unknown" when the controller hasn't been heard from lately.
	Role string `json:"role"`
	// Preferred marks the controller meant to lead whenever it's running:
	// the lowest priority among those configured with one.
	Preferred bool `json:"preferred,omitempty"`
	// Chosen marks the controller an administrator asked to lead, which
	// beats the configured priority until it's cleared.
	Chosen bool `json:"chosen,omitempty"`
	// Priority is what was configured; 0 means no preference.
	Priority int `json:"priority,omitempty"`
	// Self marks the controller answering this request.
	Self       bool      `json:"self"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// LeaderInfo names the controller doing the work, for the other one's UI.
type LeaderInfo struct {
	Name  string    `json:"name,omitempty"`
	URL   string    `json:"url,omitempty"`
	Since time.Time `json:"since,omitzero"`
	// Error is why leadership is unknown, if the database can't be reached.
	Error string `json:"error,omitempty"`
}

// leadership is this controller's role and who holds the lease.
func (s *Server) leadership() (role string, info *LeaderInfo) {
	if s.Leader == nil {
		return "single", nil
	}
	st := s.Leader.State()
	role = "standby"
	if st.Leading {
		role = "leader"
	}
	return role, &LeaderInfo{Name: st.Name, URL: st.URL, Since: st.Since, Error: st.Error}
}

// requireLeader turns away anything that changes the installation unless
// this controller is the one acting. The standby serves the same pages, so
// people can see what's happening, but the work has one owner at a time.
func (s *Server) requireLeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Leader == nil || s.Leader.State().Leading {
			next.ServeHTTP(w, r)
			return
		}
		_, info := s.leadership()
		where := "another controller"
		if info != nil && info.Name != "" {
			where = info.Name
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"message": "this controller is on standby; " + where + " is in charge",
			"leader":  info,
		})
	})
}

// ReplicationSummary counts replicas (of repositories with a primary) by
// state, and says what this controller keeps the same besides the refs, so
// the UI can tell people what replication means here rather than guessing.
type ReplicationSummary struct {
	Enabled  bool           `json:"enabled"`
	Counts   map[string]int `json:"counts"`
	Features []string       `json:"features,omitempty"`
}

// DatabaseStatus is whether the controller can reach PostgreSQL, as the
// overview reports it.
type DatabaseStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	o := Overview{
		Version:   buildinfo.Version,
		Commit:    buildinfo.Commit,
		StartedAt: s.StartedAt.UTC(),
		Database:  DatabaseStatus{OK: true},
		Nodes:     map[string]int{"total": 0},
	}
	o.Role, o.Leader = s.leadership()
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
	o.Replication = ReplicationSummary{Enabled: s.Replication != nil, Counts: map[string]int{}, Features: s.ReplicationFeatures}
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
	o.Controllers = s.controllers(ctx, o.Leader)
	if chosen, err := s.DB.Chosen(ctx); err != nil {
		s.Log.Warn("reading the chosen controller failed", "error", err)
	} else if chosen.Controller != "" {
		o.Chosen = &chosen
		for i := range o.Controllers {
			o.Controllers[i].Chosen = o.Controllers[i].Name == chosen.Controller
		}
	}
	writeJSON(w, http.StatusOK, o)
}

// controllers lists every controller sharing the database, saying which
// one is acting. A controller that hasn't written its heartbeat for
// several rounds is "unknown" rather than standby: it may be stopped, and
// saying it's ready to take over would be a promise nobody can keep.
func (s *Server) controllers(ctx context.Context, leading *LeaderInfo) []ControllerInfo {
	recs, err := s.DB.Controllers(ctx)
	if err != nil {
		s.Log.Warn("listing controllers failed", "error", err)
		return []ControllerInfo{}
	}
	// The preferred one is the lowest priority anybody configured.
	best := 0
	for _, c := range recs {
		if c.Priority > 0 && (best == 0 || c.Priority < best) {
			best = c.Priority
		}
	}
	out := make([]ControllerInfo, 0, len(recs))
	for _, c := range recs {
		info := ControllerInfo{Name: c.Name, URL: c.URL, Address: c.Address, Version: c.Version,
			StartedAt: c.StartedAt, LastSeenAt: c.LastSeenAt, Role: "standby"}
		switch {
		case time.Since(c.LastSeenAt) > s.controllerStale():
			info.Role = "unknown"
		case leading != nil && leading.Name == c.Name:
			info.Role = "leader"
		case leading == nil:
			// A single controller: it holds the lease on its own.
			info.Role = "leader"
		}
		if s.ControllerName != "" && c.Name == s.ControllerName {
			info.Self = true
		}
		info.Priority = c.Priority
		info.Preferred = c.Priority > 0 && c.Priority == best
		out = append(out, info)
	}
	return out
}

// controllerStale is how long a controller can go unheard-of before its
// role is no longer worth reporting: three heartbeats.
func (s *Server) controllerStale() time.Duration {
	if s.ControllerBeat > 0 {
		return 3 * s.ControllerBeat
	}
	return 45 * time.Second
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
		// Nothing here opens a window, and nothing of ForgeSync's should be
		// loaded by another site: a page kept in its own browsing context
		// group can't be reached by one that opened it, and a resource
		// marked same-origin can't be embedded elsewhere.
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// Everything the UI loads is its own, which the content security
		// policy already insists on, so requiring each resource to say so
		// costs nothing and puts the page in its own isolated context.
		h.Set("Cross-Origin-Embedder-Policy", "require-corp")
		// The admin UI asks the browser for none of this, so say so rather
		// than leaving it to a default that may change.
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		if r.TLS != nil {
			// Only on a request that arrived over TLS here. A plain-HTTP
			// listener may be the one a reverse proxy talks to, or a local
			// development install, and telling a browser that http://host
			// is HTTPS-only for a year is not something a plain listener
			// can take back. Where a proxy terminates TLS, the proxy is
			// the one that has to send this; deploy/prod/README.md says so.
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
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
