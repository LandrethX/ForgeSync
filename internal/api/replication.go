package api

import (
	"encoding/json"
	"net/http"

	"scenegit.org/forgesync/internal/store"
)

// replicationSources says what each node has sent: the repositories it is
// the primary of, where their copies went and when they last did.
//
// A node's page can show what it holds, and a repository's page can show
// where it went, but neither answers "what has this node sent, and is any
// of it stuck" without reading every repository. The nodes page asks this
// instead.
func (s *Server) replicationSources(w http.ResponseWriter, r *http.Request) {
	out := replicationSources{Enabled: s.Replication != nil, Pairs: []store.SourcePair{}}
	if !out.Enabled {
		writeJSON(w, http.StatusOK, out)
		return
	}
	pairs, err := s.DB.SourcePairs(r.Context())
	if err != nil {
		s.serverError(w, "list replication sources", err)
		return
	}
	if pairs != nil {
		out.Pairs = pairs
	}
	writeJSON(w, http.StatusOK, out)
}

type replicationSources struct {
	// Enabled is false when this controller doesn't replicate at all, so
	// the page says that rather than showing an empty table.
	Enabled bool               `json:"enabled"`
	Pairs   []store.SourcePair `json:"pairs"`
}

// chooseLeader: PUT /leadership {"controller": "forgesync-b"} and DELETE
// to go back to the configured order. Administrators only, and audited.
//
// It deliberately isn't behind requireLeader: the whole point is to ask
// from the controller you're looking at, which is usually the one that
// isn't leading. All it does is write the choice to the database; the
// leader reads it on its next renewal and steps aside, and the chosen one
// takes the lease from there. Nothing here takes a lease away, so two
// controllers can't both think they hold one.
func (s *Server) chooseLeader(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Controller string `json:"controller"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": `expected JSON {"controller": "<name>"}`})
		return
	}
	known, err := s.DB.Controllers(r.Context())
	if err != nil {
		s.serverError(w, "list controllers", err)
		return
	}
	var found *store.ControllerRecord
	for i, c := range known {
		if c.Name == body.Controller {
			found = &known[i]
		}
	}
	if found == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "no controller called " + body.Controller + " has checked in"})
		return
	}
	actor := identity(r).Actor()
	if err := s.DB.Choose(r.Context(), found.Name, actor); err != nil {
		s.serverError(w, "choose leader", err)
		return
	}
	s.audit(r.Context(), actor, "leadership.chosen", found.Name, map[string]any{"controller": found.Name})
	s.Log.Info("leadership chosen", "controller", found.Name, "by", actor)
	writeJSON(w, http.StatusOK, map[string]any{"controller": found.Name,
		"message": found.Name + " will take over within a lease"})
}

func (s *Server) clearLeaderChoice(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.ClearChoice(r.Context()); err != nil {
		s.serverError(w, "clear the leader choice", err)
		return
	}
	actor := identity(r).Actor()
	s.audit(r.Context(), actor, "leadership.choice_cleared", "", nil)
	writeJSON(w, http.StatusOK, map[string]string{"message": "the configured order applies again"})
}
