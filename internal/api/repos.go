package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"scenegit.org/forgesync/internal/inventory"
	"scenegit.org/forgesync/internal/store"
)

// Inventory is the repository scanner.
type Inventory interface {
	Trigger() bool
	Running() bool
	Interval() time.Duration
}

// Replicator is the replication engine.
type Replicator interface {
	Trigger(ctx context.Context, id string) (bool, error)
}

// Repository is what the repository endpoints return.
type Repository struct {
	store.RepositoryRecord
	Status      inventory.Status     `json:"status"`
	Nodes       []inventory.NodeView `json:"nodes"`
	Replication *RepoReplication     `json:"replication,omitempty"` // only on the single-repository endpoint
	// Archives are the copies kept after it was deleted on its primary
	// (single-repository endpoint only).
	Archives []store.Archive `json:"archives,omitempty"`
}

// RepoReplication is a repository's replication state per replica.
type RepoReplication struct {
	Enabled  bool                `json:"enabled"`
	Replicas []store.ReplicaSync `json:"replicas"`
}

type repositoryList struct {
	Total  int                      `json:"total"`
	Counts map[inventory.Status]int `json:"counts"` // across all repositories, ignoring filters
	Items  []Repository             `json:"items"`
}

func (s *Server) nodeNames() []string {
	names := make([]string, len(s.Nodes))
	for i, n := range s.Nodes {
		names[i] = n.Name
	}
	return names
}

func (s *Server) scansByNode(r *http.Request) (map[string]store.NodeScan, error) {
	scans, err := s.DB.NodeScans(r.Context())
	if err != nil {
		return nil, err
	}
	m := make(map[string]store.NodeScan, len(scans))
	for _, sc := range scans {
		m[sc.Node] = sc
	}
	return m, nil
}

// listRepositories: ?q= (name contains), ?status=, ?limit=, ?offset=.
func (s *Server) listRepositories(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 100, 1, 500)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	offset, err := queryInt(r, "offset", 0, 0, 1<<30)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	status := inventory.Status(r.URL.Query().Get("status"))
	switch status {
	case "", inventory.Same, inventory.Differs, inventory.Missing, inventory.Unknown, inventory.Deleted:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "status must be same, differs, missing, unknown or deleted"})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	recs, err := s.DB.Repositories(r.Context())
	if err != nil {
		s.serverError(w, "list repositories", err)
		return
	}
	scans, err := s.scansByNode(r)
	if err != nil {
		s.serverError(w, "list scans", err)
		return
	}
	// The inventory is small enough to compare in memory; move filtering into
	// SQL if installations grow to tens of thousands of repositories.
	res := repositoryList{Counts: map[inventory.Status]int{}, Items: []Repository{}}
	names := s.nodeNames()
	for _, rec := range recs {
		st, views := inventory.Compare(rec, names, scans)
		res.Counts[st]++
		if (status != "" && st != status) || (q != "" && !strings.Contains(strings.ToLower(rec.FullName), q)) {
			continue
		}
		res.Total++
		if res.Total > offset && len(res.Items) < limit {
			res.Items = append(res.Items, Repository{RepositoryRecord: rec, Status: st, Nodes: views})
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getRepository(w http.ResponseWriter, r *http.Request) {
	rec, err := s.DB.Repository(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such repository"})
		return
	}
	if err != nil {
		s.serverError(w, "get repository", err)
		return
	}
	scans, err := s.scansByNode(r)
	if err != nil {
		s.serverError(w, "list scans", err)
		return
	}
	st, views := inventory.Compare(rec, s.nodeNames(), scans)
	repl := &RepoReplication{Enabled: s.Replication != nil, Replicas: []store.ReplicaSync{}}
	if s.Replication != nil && rec.PrimaryNode != "" {
		syncs, err := s.DB.ReplicaSyncs(r.Context(), rec.ID)
		if err != nil {
			s.serverError(w, "get replication state", err)
			return
		}
		for _, x := range syncs {
			if x.Node != rec.PrimaryNode { // rows left from before a primary change
				repl.Replicas = append(repl.Replicas, x)
			}
		}
	}
	archives, err := s.DB.Archives(r.Context(), rec.ID)
	if err != nil {
		s.serverError(w, "get archives", err)
		return
	}
	writeJSON(w, http.StatusOK, Repository{RepositoryRecord: rec, Status: st, Nodes: views, Replication: repl, Archives: archives})
}

// replicateNow starts replicating one repository. Operators and up.
func (s *Server) replicateNow(w http.ResponseWriter, r *http.Request) {
	if s.Replication == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "replication is turned off on this controller (replication.enabled)"})
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.DB.Repository(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such repository"})
		return
	}
	if err != nil {
		s.serverError(w, "get repository", err)
		return
	}
	if rec.PrimaryNode == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "set a primary before replicating"})
		return
	}
	queued, err := s.Replication.Trigger(r.Context(), id)
	if err != nil {
		s.serverError(w, "start replication", err)
		return
	}
	if queued {
		s.audit(r.Context(), identity(r).Actor(), "repo.replicate_requested", rec.FullName, map[string]any{"repository_id": id})
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": queued, "running": !queued})
}

// setPrimary: PUT {"node": "se"}. Administrators only. Every repository has a
// primary (by default its origin, set after a scan), so it can be changed but
// not cleared. With replication on, it's where branches and tags are copied from.
func (s *Server) setPrimary(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node *string `json:"node"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Node == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": `expected JSON {"node": "<name>"}`})
		return
	}
	node := *body.Node
	if node == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "a repository's primary can be changed but not cleared"})
		return
	}
	if !contains(s.nodeNames(), node) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "no node named " + node})
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.DB.Repository(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such repository"})
		return
	}
	if err != nil {
		s.serverError(w, "get repository", err)
		return
	}
	prev, err := s.DB.SetPrimary(r.Context(), id, node)
	if err != nil {
		s.serverError(w, "set primary", err)
		return
	}
	if prev != node {
		s.audit(r.Context(), identity(r).Actor(), "repo.set_primary", rec.FullName,
			map[string]any{"repository_id": id, "from": prev, "to": node})
	}
	writeJSON(w, http.StatusOK, map[string]string{"primary_node": node, "previous": prev})
}

type inventoryStatus struct {
	Running         bool             `json:"running"`
	IntervalSeconds int              `json:"interval_seconds"`
	Nodes           []store.NodeScan `json:"nodes"`
}

func (s *Server) inventoryStatus(w http.ResponseWriter, r *http.Request) {
	scans, err := s.DB.NodeScans(r.Context())
	if err != nil {
		s.serverError(w, "list scans", err)
		return
	}
	sort.Slice(scans, func(i, j int) bool { return scans[i].Node < scans[j].Node })
	writeJSON(w, http.StatusOK, inventoryStatus{
		Running:         s.Inventory.Running(),
		IntervalSeconds: int(s.Inventory.Interval().Seconds()),
		Nodes:           scans,
	})
}

// scanNow asks for an inventory scan. Operators and up.
func (s *Server) scanNow(w http.ResponseWriter, r *http.Request) {
	started := s.Inventory.Trigger()
	if started {
		s.audit(r.Context(), identity(r).Actor(), "inventory.scan_requested", "", nil)
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": started, "running": s.Inventory.Running()})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
