// Package api serves the controller's HTTP interface:
//
//	/healthz   liveness, always 200 while the process runs
//	/readyz    readiness, 200 only when the database answers
//	/api/v1/*  admin API, bearer token (SceneID/OIDC for people comes later)
//
// The controller-to-controller (/internal/v1) and agent (/agent/v1) APIs from
// the architecture document are not part of this yet.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"scenegit.org/forgesync/internal/buildinfo"
	"scenegit.org/forgesync/internal/health"
)

// Pinger is the database check behind /readyz.
type Pinger interface {
	Ping(ctx context.Context) error
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
	AdminToken string // empty disables /api/v1
	Nodes      []NodeInfo
	Health     interface{ Snapshot() []health.Status }
	DB         Pinger
	Log        *slog.Logger
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, s.logRequests, middleware.Recoverer, securityHeaders)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.readyz)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.requireAdminToken)
		r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit})
		})
		r.Get("/nodes", s.listNodes)
	})
	return r
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
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
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) requireAdminToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.AdminToken == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "admin API disabled: http.admin_token_file is not set"})
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="forgesync"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "missing or invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
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
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
