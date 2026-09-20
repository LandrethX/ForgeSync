package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"scenegit.org/forgesync/internal/nodes"
)

// Adding a node from here rather than from a file on every controller.
//
// These are not behind requireLeader, for the same reason ForgeSync's own
// accounts are not: a node lives in the shared database, both controllers
// read it, and the person doing the adding is looking at whichever
// controller they happened to open. Nothing here touches a Forgejo node
// except to ask it what it is.
//
// What ForgeSync will not do is set the node up. The parts that matter
// most, the app.ini keys, cannot be reached through the API at all and
// need the node restarted, so the wizard says what to do and then checks.

// NodeAdmin adds and retires nodes. It is nil when replication is off or
// when no node key is configured, and then these endpoints say so rather
// than half working.
type NodeAdmin interface {
	Check(ctx context.Context, n nodes.NewNode) (nodes.Report, error)
	Add(ctx context.Context, n nodes.NewNode, by string) (nodes.Report, error)
}

// message is the shape every refusal here takes, matching the rest of
// the API.
func message(w http.ResponseWriter, status int, text string) {
	writeJSON(w, status, map[string]string{"message": text})
}

func (s *Server) nodeAdminReady(w http.ResponseWriter) bool {
	if s.NodeAdmin == nil {
		message(w, http.StatusServiceUnavailable,
			"this controller cannot add nodes: set node_key_file in its config, so a node's token can be sealed before it is stored")
		return false
	}
	return true
}

func (s *Server) readNode(w http.ResponseWriter, r *http.Request) (nodes.NewNode, bool) {
	var n nodes.NewNode
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&n); err != nil {
		message(w, http.StatusBadRequest, "the request body is not the JSON this expects")
		return n, false
	}
	if err := n.Validate(); err != nil {
		message(w, http.StatusBadRequest, err.Error())
		return n, false
	}
	return n, true
}

// checkNode asks the node what it is and says what it found, writing
// nothing. It is the Verify button: run it as often as you like while
// working through what the node still needs.
func (s *Server) checkNode(w http.ResponseWriter, r *http.Request) {
	if !s.nodeAdminReady(w) {
		return
	}
	n, ok := s.readNode(w, r)
	if !ok {
		return
	}
	rep, err := s.NodeAdmin.Check(r.Context(), n)
	if err != nil {
		message(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// createNode checks the node and, if it can be used, stores it with its
// token sealed. A node that fails a blocking check is answered with the
// report and a 422: nothing was written, and the report says why.
func (s *Server) createNode(w http.ResponseWriter, r *http.Request) {
	if !s.nodeAdminReady(w) {
		return
	}
	n, ok := s.readNode(w, r)
	if !ok {
		return
	}
	actor := identity(r).Actor()
	rep, err := s.NodeAdmin.Add(r.Context(), n, actor)
	switch {
	case errors.Is(err, nodes.ErrNoKey):
		message(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		s.serverError(w, "add node", err)
		return
	}
	if !rep.OK {
		writeJSON(w, http.StatusUnprocessableEntity, rep)
		return
	}
	// The token is never audited, only that a node was added and by whom.
	s.audit(r.Context(), actor, "node.added", n.Name, map[string]any{
		"url": n.URL, "site": n.Site, "service_user": rep.ServiceUser,
		"sceneid_source_id": n.SceneIDSourceID,
	})
	writeJSON(w, http.StatusCreated, rep)
}

// retireNode takes a node out of the installation. The row stays, because
// its health transitions are part of the history and nothing may edit
// that, and because a node taken out and put back should not come back a
// stranger.
func (s *Server) retireNode(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.DB.RetireNode(r.Context(), name); err != nil {
		message(w, http.StatusNotFound, err.Error())
		return
	}
	s.audit(r.Context(), identity(r).Actor(), "node.retired", name, nil)
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "retired",
		"note":   "nothing watches, scans or replicates to it now. What is known about its repositories is kept, and adding it again brings it back.",
	})
}
