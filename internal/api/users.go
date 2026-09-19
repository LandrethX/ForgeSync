package api

import (
	"context"
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

// UserProvisioner creates the Forgejo accounts a SceneID person has on
// the nodes. It's the replication engine; nil when replication is off,
// since without node tokens ForgeSync can't create anything.
type UserProvisioner interface {
	// CreateUser makes the account on every node, home counting as where
	// they registered. It returns the nodes it was created on and, per
	// node, why it wasn't created where it wasn't.
	CreateUser(ctx context.Context, login, subject, fullName, email, home string) ([]string, map[string]string, error)
	// ProvisionUser copies an account someone already has to the nodes
	// that haven't got it.
	ProvisionUser(ctx context.Context, login string) ([]string, map[string]string, error)
}

// createUser: POST /users. Administrators only.
//
// People come from SceneID, so this is not a way to make a local account:
// there's no password, and what's created on each node is an account that
// signs in through SceneID, linked by the subject. It's for the person
// who hasn't signed in yet -- a new member being set up -- and for making
// someone exist on the other nodes before anything of theirs is copied.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Login    string `json:"login"`
		Subject  string `json:"subject"`
		FullName string `json:"full_name"`
		Email    string `json:"email"`
		Home     string `json:"home"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": `expected JSON {"login": "...", "subject": "...", "home": "<node>"}`})
		return
	}
	body.Login, body.Subject = strings.TrimSpace(body.Login), strings.TrimSpace(body.Subject)
	switch {
	case s.Users == nil:
		writeJSON(w, http.StatusConflict, map[string]string{
			"message": "this controller can't create accounts: replication is off, so it has no tokens for the nodes"})
		return
	case body.Login == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "a username is needed"})
		return
	case body.Subject == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "a SceneID subject is needed: it's what links the account to the person when they sign in"})
		return
	case body.Home != "" && !contains(s.nodeNames(), body.Home):
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "no node named " + body.Home})
		return
	}
	created, refused, err := s.Users.CreateUser(r.Context(), body.Login, body.Subject, body.FullName, body.Email, body.Home)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	s.audit(r.Context(), identity(r).Actor(), "user.created", body.Login,
		map[string]any{"subject": body.Subject, "home": body.Home, "nodes": created, "refused": refused})
	// The user list is built from what the scan found, so ask for one.
	if s.Inventory != nil {
		s.Inventory.Trigger()
	}
	writeJSON(w, http.StatusCreated, userWrite{Login: body.Login, Created: created, Refused: refused})
}

// provisionUser: POST /users/{id}/nodes. Administrators only. Copies the
// account to the nodes that haven't got it.
func (s *Server) provisionUser(w http.ResponseWriter, r *http.Request) {
	if s.Users == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"message": "this controller can't create accounts: replication is off, so it has no tokens for the nodes"})
		return
	}
	u, err := s.DB.User(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such user"})
		return
	}
	if err != nil {
		s.serverError(w, "get user", err)
		return
	}
	created, refused, err := s.Users.ProvisionUser(r.Context(), u.Login)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	s.audit(r.Context(), identity(r).Actor(), "user.provisioned", u.Login,
		map[string]any{"user_id": u.ID, "nodes": created, "refused": refused})
	if s.Inventory != nil {
		s.Inventory.Trigger()
	}
	writeJSON(w, http.StatusOK, userWrite{Login: u.Login, Created: created, Refused: refused})
}

// userWrite is what happened on each node.
type userWrite struct {
	Login   string            `json:"login"`
	Created []string          `json:"created"`
	Refused map[string]string `json:"refused,omitempty"`
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
