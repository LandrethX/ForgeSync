package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"scenegit.org/forgesync/internal/store"
	"scenegit.org/forgesync/internal/webhook"
)

type userList struct {
	Total int                `json:"total"`
	Items []store.UserRecord `json:"items"`
}

// listUsers: GET /users?q=text. Every SceneID user ForgeSync has seen on a
// node, with their primary site.
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	all, err := s.DB.Users(r.Context())
	if err != nil {
		s.serverError(w, "list users", err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	items := []store.UserRecord{}
	for _, u := range all {
		if q == "" || strings.Contains(strings.ToLower(u.Login), q) {
			items = append(items, u)
		}
	}
	writeJSON(w, http.StatusOK, userList{Total: len(items), Items: items})
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.DB.User(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such user"})
		return
	}
	if err != nil {
		s.serverError(w, "get user", err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// setUserHome: PUT {"node": "dk"}. Administrators only. The user's
// repositories follow after the next scan, except those whose primary an
// Administrator chose.
func (s *Server) setUserHome(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Node == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": `expected JSON {"node": "<name>"}`})
		return
	}
	if !contains(s.nodeNames(), body.Node) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "no node named " + body.Node})
		return
	}
	id := chi.URLParam(r, "id")
	u, err := s.DB.User(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such user"})
		return
	}
	if err != nil {
		s.serverError(w, "get user", err)
		return
	}
	prev, err := s.DB.SetUserHome(r.Context(), id, body.Node)
	if err != nil {
		s.serverError(w, "set user home", err)
		return
	}
	if prev != body.Node {
		s.audit(r.Context(), identity(r).Actor(), "user.set_home", u.Login,
			map[string]any{"user_id": id, "from": prev, "to": body.Node})
	}
	writeJSON(w, http.StatusOK, map[string]string{"home_node": body.Node, "previous": prev})
}

type webhookStatus struct {
	Enabled bool             `json:"enabled"`
	Nodes   []webhook.Status `json:"nodes"`
}

// webhookStatus: GET /webhooks. Whether each node's webhook is installed and
// delivering.
func (s *Server) webhookStatus(w http.ResponseWriter, _ *http.Request) {
	if s.WebhookStatus == nil {
		writeJSON(w, http.StatusOK, webhookStatus{Nodes: []webhook.Status{}})
		return
	}
	writeJSON(w, http.StatusOK, webhookStatus{Enabled: true, Nodes: s.WebhookStatus.Snapshot()})
}
