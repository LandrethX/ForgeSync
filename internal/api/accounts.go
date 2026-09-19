package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"scenegit.org/forgesync/internal/auth"
	"scenegit.org/forgesync/internal/store"
)

// ForgeSync's own accounts: people who sign in to the controllers
// themselves, as opposed to the SceneID users who sign in to the Forgejo
// nodes. They exist only in ForgeSync's database, which both controllers
// share, so an account made on either works on both -- and keeps working
// when SceneID is the thing that's unreachable, which is when someone
// most needs to get in. Nothing replicates them to a node; a node never
// learns they exist.
//
// The first one has to be made with the admin token, since there's nobody
// to make it otherwise. After that an administrator manages them here.

// minPasswordLength is the only rule. Length is what makes a password
// hard to guess; the rest is decoration people work around.
const minPasswordLength = 12

// accountRole turns what was asked for into a role, in the words a
// person would use rather than the parser's.
func accountRole(name string) (auth.Role, error) {
	role, err := auth.ParseRole(strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return auth.NoRole, errors.New("the role must be viewer, operator or administrator")
	}
	return role, nil
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.DB.Accounts(r.Context())
	if err != nil {
		s.serverError(w, "list accounts", err)
		return
	}
	if accounts == nil {
		accounts = []store.Account{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(accounts), "items": accounts})
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": `expected JSON {"username": "...", "password": "...", "role": "..."}`})
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	role, err := accountRole(body.Role)
	switch {
	case body.Username == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "a username is needed"})
		return
	case strings.ContainsAny(body.Username, " \t/:@"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "the username can't contain spaces or / : @"})
		return
	case len(body.Password) < minPasswordLength:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "the password must be at least " + strconv.Itoa(minPasswordLength) + " characters"})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	actor := identity(r).Actor()
	a, err := s.DB.CreateAccount(r.Context(), body.Username, body.Password, strings.TrimSpace(body.FullName), role.String(), actor)
	if errors.Is(err, store.ErrUsernameTaken) {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "that username is taken"})
		return
	}
	if err != nil {
		s.serverError(w, "create account", err)
		return
	}
	s.audit(r.Context(), actor, "account.created", a.Username, map[string]any{"role": a.Role, "account_id": a.ID})
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FullName *string `json:"full_name"`
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "expected a JSON object"})
		return
	}
	id := chi.URLParam(r, "id")
	current, err := s.DB.Account(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such account"})
		return
	}
	if err != nil {
		s.serverError(w, "get account", err)
		return
	}
	name, role, disabled := current.FullName, current.Role, current.Disabled
	if body.FullName != nil {
		name = strings.TrimSpace(*body.FullName)
	}
	if body.Role != nil {
		parsed, err := accountRole(*body.Role)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		role = parsed.String()
	}
	if body.Disabled != nil {
		disabled = *body.Disabled
	}
	// Don't let the last administrator lock everyone out.
	if (role != auth.Administrator.String() || disabled) && current.Role == auth.Administrator.String() && !current.Disabled {
		if only, err := s.lastAdministrator(r.Context(), current.ID); err != nil {
			s.serverError(w, "count administrators", err)
			return
		} else if only {
			writeJSON(w, http.StatusConflict, map[string]string{
				"message": "this is the only administrator account that can sign in; make another one first"})
			return
		}
	}
	a, err := s.DB.UpdateAccount(r.Context(), id, name, role, disabled)
	if err != nil {
		s.serverError(w, "update account", err)
		return
	}
	actor := identity(r).Actor()
	s.audit(r.Context(), actor, "account.updated", a.Username,
		map[string]any{"role": a.Role, "disabled": a.Disabled, "account_id": a.ID})
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) setAccountPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": `expected JSON {"password": "..."}`})
		return
	}
	if len(body.Password) < minPasswordLength {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"message": "the password must be at least " + strconv.Itoa(minPasswordLength) + " characters"})
		return
	}
	id := chi.URLParam(r, "id")
	a, err := s.DB.Account(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such account"})
		return
	}
	if err != nil {
		s.serverError(w, "get account", err)
		return
	}
	if err := s.DB.SetAccountPassword(r.Context(), id, body.Password); err != nil {
		s.serverError(w, "set the password", err)
		return
	}
	actor := identity(r).Actor()
	s.audit(r.Context(), actor, "account.password_set", a.Username, map[string]any{"account_id": a.ID})
	writeJSON(w, http.StatusOK, map[string]string{"message": "the password was changed"})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := s.DB.Account(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such account"})
		return
	}
	if err != nil {
		s.serverError(w, "get account", err)
		return
	}
	if a.Role == auth.Administrator.String() && !a.Disabled {
		if only, err := s.lastAdministrator(r.Context(), a.ID); err != nil {
			s.serverError(w, "count administrators", err)
			return
		} else if only {
			writeJSON(w, http.StatusConflict, map[string]string{
				"message": "this is the only administrator account that can sign in; make another one first"})
			return
		}
	}
	if err := s.DB.DeleteAccount(r.Context(), id); err != nil {
		s.serverError(w, "delete account", err)
		return
	}
	actor := identity(r).Actor()
	s.audit(r.Context(), actor, "account.deleted", a.Username, map[string]any{"account_id": a.ID})
	writeJSON(w, http.StatusOK, map[string]string{"message": a.Username + " was removed"})
}

// lastAdministrator reports that the account is the only one that can
// still sign in and manage the others.
func (s *Server) lastAdministrator(ctx context.Context, except string) (bool, error) {
	all, err := s.DB.Accounts(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range all {
		if a.ID != except && a.Role == auth.Administrator.String() && !a.Disabled {
			return false, nil
		}
	}
	return true, nil
}
