package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"scenegit.org/forgesync/internal/store"
)

type conflictList struct {
	Total  int              `json:"total"`
	Counts map[string]int   `json:"counts"`
	Items  []store.Conflict `json:"items"`
}

// listConflicts: ?state=open|cleared|all (default open), ?repository=<id>,
// ?limit=, ?offset=.
func (s *Server) listConflicts(w http.ResponseWriter, r *http.Request) {
	f := store.ConflictFilter{RepositoryID: r.URL.Query().Get("repository")}
	switch st := r.URL.Query().Get("state"); st {
	case "", "open":
		f.State = "open"
	case "cleared":
		f.State = st
	case "all":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "state must be open, cleared or all"})
		return
	}
	var err error
	if f.Limit, err = queryInt(r, "limit", 100, 1, 500); err == nil {
		f.Offset, err = queryInt(r, "offset", 0, 0, 1<<30)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	items, total, counts, err := s.DB.Conflicts(r.Context(), f)
	if err != nil {
		s.serverError(w, "list conflicts", err)
		return
	}
	writeJSON(w, http.StatusOK, conflictList{Total: total, Counts: counts, Items: items})
}

func (s *Server) conflictID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such conflict"})
		return 0, false
	}
	return id, true
}

func (s *Server) getConflict(w http.ResponseWriter, r *http.Request) {
	id, ok := s.conflictID(w, r)
	if !ok {
		return
	}
	c, err := s.DB.ConflictByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such conflict"})
		return
	}
	if err != nil {
		s.serverError(w, "get conflict", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

const maxNote = 1000

// acknowledgeConflict records that someone is looking at a conflict, with an
// optional note. Operators and up. It doesn't change any data on the nodes.
func (s *Server) acknowledgeConflict(w http.ResponseWriter, r *http.Request) {
	id, ok := s.conflictID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": `expected JSON {"note": "..."}`})
		return
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > maxNote {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "the note can be at most 1000 characters"})
		return
	}
	c, err := s.DB.ConflictByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such conflict"})
		return
	}
	if err != nil {
		s.serverError(w, "get conflict", err)
		return
	}
	actor := identity(r).Actor()
	if err := s.DB.AcknowledgeConflict(r.Context(), id, actor, note, time.Now().UTC()); err != nil {
		s.serverError(w, "acknowledge conflict", err)
		return
	}
	s.audit(r.Context(), actor, "conflict.acknowledged", c.FullName,
		map[string]any{"conflict_id": id, "kind": c.Kind, "ref": c.Ref, "note": note})
	c, _ = s.DB.ConflictByID(r.Context(), id)
	writeJSON(w, http.StatusOK, c)
}
