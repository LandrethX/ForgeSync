// Package forgejo is a small client for the Forgejo REST API.
//
// It covers only what ForgeSync uses and grows with it. Check request and
// response fields against the Forgejo v16 source (modules/structs) before
// adding them; Gitea-era docs are often wrong in the details.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/buildinfo"
)

// Client talks to one Forgejo node. It is safe for concurrent use.
type Client struct {
	base  *url.URL
	token string
	sudo  string
	http  *http.Client
}

// New returns a client for the Forgejo instance at baseURL, authenticating
// with an API token.
func New(baseURL, token string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
			// Forgejo redirects an old repository name after a rename or
			// transfer; ForgeSync means the exact name, so a redirect is
			// "not here" (see isNotFound), not the repository it points to.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{base: u, token: token, http: httpClient}, nil
}

// Sudo returns a copy of the client whose requests act as the given user.
// Forgejo allows this only for site-admin tokens.
func (c *Client) Sudo(user string) *Client {
	cp := *c
	cp.sudo = user
	return &cp
}

// APIError is a non-2xx response from Forgejo.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("forgejo %s %s: %d %s", e.Method, e.Path, e.StatusCode, msg)
}

// IsAuthError reports whether err is a 401 or 403 from Forgejo.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden)
}

// do sends a request to path (relative to the instance root) and decodes a
// JSON response into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, auth bool, in, out any) error {
	return c.doWith(ctx, method, path, auth, in, out, nil)
}

// doWith is do, additionally passing the response headers of a successful
// response to onHeader.
func (c *Client) doWith(ctx context.Context, method, path string, auth bool, in, out any, onHeader func(http.Header)) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "forgesync/"+buildinfo.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "token "+c.token)
		if c.sudo != "" {
			req.Header.Set("Sudo", c.sudo)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Method: method, Path: path, StatusCode: resp.StatusCode}
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &m) == nil {
			apiErr.Message = m.Message
		}
		return apiErr
	}
	if onHeader != nil {
		onHeader(resp.Header)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("forgejo %s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Healthz is the response of /api/healthz.
type Healthz struct {
	Status      string `json:"status"` // pass, warn or fail
	Description string `json:"description"`
}

// Healthz checks Forgejo's own health endpoint. It needs no token.
func (c *Client) Healthz(ctx context.Context) (Healthz, error) {
	var h Healthz
	err := c.do(ctx, http.MethodGet, "/api/healthz", false, nil, &h)
	return h, err
}

// Version returns the Forgejo version string, e.g. "16.0.5+gitea-1.22.0".
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/version", false, nil, &v)
	return v.Version, err
}

// User is a Forgejo account. LoginName and SourceID are only filled in for
// admin callers; for SceneID users LoginName holds the OIDC subject.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Email     string `json:"email"`
	FullName  string `json:"full_name"`
	IsAdmin   bool   `json:"is_admin"`
	SourceID  int64  `json:"source_id"`
	LoginName string `json:"login_name"`
	// Visibility is public, limited or private.
	Visibility string    `json:"visibility"`
	Created    time.Time `json:"created"`
}

// CurrentUser returns the account the token (or sudo user) acts as.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/api/v1/user", true, nil, &u)
	return u, err
}

// Repository is a Forgejo repository as listed by /repos/search.
type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Owner         User   `json:"owner"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Mirror        bool   `json:"mirror"`
	Archived      bool   `json:"archived"`
	Empty         bool   `json:"empty"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
	Template      bool   `json:"template"`
	// ObjectFormatName is sha1 or sha256; a copy must use the same.
	ObjectFormatName string    `json:"object_format_name"`
	Size             int       `json:"size"`
	Updated          time.Time `json:"updated_at"`
	Created          time.Time `json:"created_at"`
}

// ListRepos returns one page of all repositories the token can see (for a
// site admin: every repository) and the total count across pages.
func (c *Client) ListRepos(ctx context.Context, page, limit int) ([]Repository, int, error) {
	var res struct {
		OK   bool         `json:"ok"`
		Data []Repository `json:"data"`
	}
	path := fmt.Sprintf("/api/v1/repos/search?page=%d&limit=%d&sort=id&order=asc", page, limit)
	total, err := c.doCounted(ctx, path, &res)
	return res.Data, total, err
}

// BranchHead returns the commit a branch points to.
func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	// Forgejo matches the branch with a wildcard route, so slashes in branch
	// names stay slashes; each segment is escaped on its own.
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	path := "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/branches/" + strings.Join(segs, "/")
	err := c.do(ctx, http.MethodGet, path, true, nil, &b)
	return b.Commit.ID, err
}

// doCounted is an authenticated GET that also returns X-Total-Count.
func (c *Client) doCounted(ctx context.Context, path string, out any) (int, error) {
	var total int
	err := c.doWith(ctx, http.MethodGet, path, true, nil, out, func(h http.Header) {
		total, _ = strconv.Atoi(h.Get("X-Total-Count"))
	})
	return total, err
}

// CommitsAhead reports how many commits are reachable from head but not from
// base, in one repository. found is false if either commit doesn't exist
// there (Forgejo answers 404). Commit IDs, branches and tags all work.
func (c *Client) CommitsAhead(ctx context.Context, owner, repo, base, head string) (n int, found bool, err error) {
	var res struct {
		TotalCommits int `json:"total_commits"`
	}
	// Skip per-commit file lists and signature checks; only the count matters.
	path := "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/compare/" +
		url.PathEscape(base) + "..." + url.PathEscape(head) + "?files=false&verification=false"
	err = c.do(ctx, http.MethodGet, path, true, nil, &res)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return res.TotalCommits, true, nil
}

// isNotFound reports whether err is a 404 from Forgejo, or a redirect: an
// old name that now points elsewhere isn't the repository asked for.
func isNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusNotFound, http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// IsConflict reports whether err is a 409 or 422 from Forgejo, which it
// returns when something being created already exists.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusUnprocessableEntity)
}

// GetUser returns a user by login name; found is false if there's none.
// With a site-admin token, LoginName and SourceID are filled in.
func (c *Client) GetUser(ctx context.Context, login string) (u User, found bool, err error) {
	err = c.do(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(login), true, nil, &u)
	if isNotFound(err) {
		return User{}, false, nil
	}
	return u, err == nil, err
}

// IsOrg reports whether an organization with this name exists.
func (c *Client) IsOrg(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(name), true, nil, nil)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// GetRepo returns a repository; found is false if there's none.
func (c *Client) GetRepo(ctx context.Context, owner, name string) (r Repository, found bool, err error) {
	err = c.do(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), true, nil, &r)
	if isNotFound(err) {
		return Repository{}, false, nil
	}
	return r, err == nil, err
}

// UsersByLoginName returns the users of a login source with this login name
// (for SceneID users, the OIDC subject). Site admin only.
func (c *Client) UsersByLoginName(ctx context.Context, sourceID int64, loginName string) ([]User, error) {
	var us []User
	path := fmt.Sprintf("/api/v1/admin/users?source_id=%d&login_name=%s&limit=50", sourceID, url.QueryEscape(loginName))
	err := c.do(ctx, http.MethodGet, path, true, nil, &us)
	return us, err
}

// CreateUserOption is the body of POST /admin/users (modules/structs/admin_user.go).
type CreateUserOption struct {
	SourceID           int64      `json:"source_id"`
	LoginName          string     `json:"login_name"`
	Username           string     `json:"username"`
	FullName           string     `json:"full_name,omitempty"`
	Email              string     `json:"email"`
	MustChangePassword *bool      `json:"must_change_password,omitempty"`
	Visibility         string     `json:"visibility,omitempty"`
	Created            *time.Time `json:"created_at,omitempty"`
}

// AdminCreateUser creates an account. Site admin only.
func (c *Client) AdminCreateUser(ctx context.Context, opt CreateUserOption) (User, error) {
	var u User
	err := c.do(ctx, http.MethodPost, "/api/v1/admin/users", true, opt, &u)
	return u, err
}

// CreateRepoOption is the body of repository creation (modules/structs/repo.go).
type CreateRepoOption struct {
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	Private          bool   `json:"private"`
	Template         bool   `json:"template,omitempty"`
	DefaultBranch    string `json:"default_branch,omitempty"`
	ObjectFormatName string `json:"object_format_name,omitempty"`
}

// AdminCreateRepo creates an empty repository owned by a user or an
// organization. Site admin only.
func (c *Client) AdminCreateRepo(ctx context.Context, owner string, opt CreateRepoOption) (Repository, error) {
	var r Repository
	err := c.do(ctx, http.MethodPost, "/api/v1/admin/users/"+url.PathEscape(owner)+"/repos", true, opt, &r)
	return r, err
}

// ListUsers returns one page of the accounts of one login source (not
// organizations), oldest first, and the total count across pages. Site admin
// only; LoginName and SourceID are filled in.
func (c *Client) ListUsers(ctx context.Context, sourceID int64, page, limit int) ([]User, int, error) {
	var us []User
	path := fmt.Sprintf("/api/v1/admin/users?source_id=%d&page=%d&limit=%d&sort=oldest", sourceID, page, limit)
	total, err := c.doCounted(ctx, path, &us)
	return us, total, err
}

// PullRequest is the part of a Forgejo pull request ForgeSync reads.
type PullRequest struct {
	Number  int64     `json:"number"`
	HTMLURL string    `json:"html_url"`
	State   string    `json:"state"` // open or closed
	Merged  bool      `json:"merged"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	Draft   bool      `json:"draft"`
	User    User      `json:"user"`
	Head    *PRBranch `json:"head"`
	Base    *PRBranch `json:"base"`
}

// PRBranch is one side of a pull request.
type PRBranch struct {
	Ref string `json:"ref"` // the branch name
	SHA string `json:"sha"`
}

// BranchOf is a pull request's branch name on one side, or "" if Forgejo
// didn't say: the branch is gone, or it came from a fork.
func BranchOf(b *PRBranch) string {
	if b == nil {
		return ""
	}
	return b.Ref
}

// CreatePullRequestOption is the body of POST /repos/{owner}/{repo}/pulls
// (modules/structs/pull.go).
type CreatePullRequestOption struct {
	Head      string   `json:"head"`
	Base      string   `json:"base"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Assignees []string `json:"assignees,omitempty"`
}

func repoPath(owner, repo string) string {
	return "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// CreatePullRequest opens a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, owner, repo string, opt CreatePullRequestOption) (PullRequest, error) {
	var pr PullRequest
	err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/pulls", true, opt, &pr)
	return pr, err
}

// GetPullRequest returns a pull request; found is false if it's gone.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int64) (pr PullRequest, found bool, err error) {
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), true, nil, &pr)
	if isNotFound(err) {
		return PullRequest{}, false, nil
	}
	return pr, err == nil, err
}

// Comment adds a comment to an issue or pull request.
func (c *Client) Comment(ctx context.Context, owner, repo string, number int64, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repoPath(owner, repo), number), true,
		map[string]string{"body": body}, nil)
}

// Org is an organization as ForgeSync keeps it the same everywhere.
type Org struct {
	Name        string `json:"username"`
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	Website     string `json:"website"`
	Location    string `json:"location"`
	// Visibility is public, limited or private.
	Visibility string `json:"visibility"`
}

// Team is one of an organization's teams. Units are the parts of a
// repository it reaches ("repo.code", "repo.issues", ...).
type Team struct {
	ID                      int64    `json:"id"`
	Name                    string   `json:"name"`
	Description             string   `json:"description"`
	Permission              string   `json:"permission"` // none, read, write, admin, owner
	Units                   []string `json:"units"`
	CanCreateOrgRepo        bool     `json:"can_create_org_repo"`
	IncludesAllRepositories bool     `json:"includes_all_repositories"`
}

// GetOrg returns an organization; found is false if the node hasn't got it.
func (c *Client) GetOrg(ctx context.Context, name string) (Org, bool, error) {
	var o Org
	err := c.do(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(name), true, nil, &o)
	if isNotFound(err) {
		return Org{}, false, nil
	}
	return o, err == nil, err
}

// EditOrg changes the given fields of an organization.
func (c *Client) EditOrg(ctx context.Context, name string, fields map[string]any) error {
	return c.do(ctx, http.MethodPatch, "/api/v1/orgs/"+url.PathEscape(name), true, fields, nil)
}

// OrgTeams lists an organization's teams.
func (c *Client) OrgTeams(ctx context.Context, org string) ([]Team, error) {
	var out []Team
	err := c.do(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(org)+"/teams?limit=100", true, nil, &out)
	return out, err
}

// CreateTeam adds a team to an organization.
func (c *Client) CreateTeam(ctx context.Context, org string, t Team) (Team, error) {
	var out Team
	err := c.do(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(org)+"/teams", true, teamBody(t), &out)
	return out, err
}

// EditTeam changes a team.
func (c *Client) EditTeam(ctx context.Context, id int64, t Team) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/teams/%d", id), true, teamBody(t), nil)
}

func teamBody(t Team) map[string]any {
	return map[string]any{"name": t.Name, "description": t.Description, "permission": t.Permission,
		"units": t.Units, "can_create_org_repo": t.CanCreateOrgRepo,
		"includes_all_repositories": t.IncludesAllRepositories}
}

// DeleteTeam removes a team. Already gone is not an error.
func (c *Client) DeleteTeam(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/teams/%d", id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// TeamMembers lists who is in a team.
func (c *Client) TeamMembers(ctx context.Context, id int64) ([]User, error) {
	var out []User
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/teams/%d/members?limit=100", id), true, nil, &out)
	return out, err
}

// AddTeamMember puts someone in a team.
func (c *Client) AddTeamMember(ctx context.Context, id int64, login string) error {
	return c.do(ctx, http.MethodPut,
		fmt.Sprintf("/api/v1/teams/%d/members/%s", id, url.PathEscape(login)), true, nil, nil)
}

// RemoveTeamMember takes them out again. Already gone is not an error.
func (c *Client) RemoveTeamMember(ctx context.Context, id int64, login string) error {
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/api/v1/teams/%d/members/%s", id, url.PathEscape(login)), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Collaborators lists the people a repository is shared with, which is not
// the same as who can see it: the owner and site admins aren't in here.
func (c *Client) Collaborators(ctx context.Context, owner, repo string) ([]User, error) {
	var out []User
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/collaborators", true, nil, &out)
	return out, err
}

// CollaboratorPermission is one collaborator's access: read, write or
// admin.
func (c *Client) CollaboratorPermission(ctx context.Context, owner, repo, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/collaborators/%s/permission", repoPath(owner, repo), url.PathEscape(login)), true, nil, &out)
	return out.Permission, err
}

// AddCollaborator shares a repository with someone, or changes what they
// may do; permission is read, write or admin.
func (c *Client) AddCollaborator(ctx context.Context, owner, repo, login, permission string) error {
	return c.do(ctx, http.MethodPut,
		fmt.Sprintf("%s/collaborators/%s", repoPath(owner, repo), url.PathEscape(login)), true,
		map[string]string{"permission": permission}, nil)
}

// RemoveCollaborator stops sharing it. Already gone is not an error.
func (c *Client) RemoveCollaborator(ctx context.Context, owner, repo, login string) error {
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("%s/collaborators/%s", repoPath(owner, repo), url.PathEscape(login)), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// BranchExists reports whether a branch is on the node. ForgeSync opens a
// copy of a pull request only once both its branches are there.
func (c *Client) BranchExists(ctx context.Context, owner, repo, branch string) (bool, error) {
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/branches/%s", repoPath(owner, repo), url.PathEscape(branch)),
		true, nil, nil)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// SetDefaultBranch changes a repository's default branch.
func (c *Client) SetDefaultBranch(ctx context.Context, owner, repo, branch string) error {
	return c.do(ctx, http.MethodPatch, repoPath(owner, repo), true, map[string]string{"default_branch": branch}, nil)
}

// OrgOwners returns the members of an organization's owner teams.
func (c *Client) OrgOwners(ctx context.Context, org string) ([]string, error) {
	var teams []struct {
		ID         int64  `json:"id"`
		Permission string `json:"permission"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(org)+"/teams?limit=50", true, nil, &teams); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range teams {
		if t.Permission != "owner" {
			continue
		}
		var members []User
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/teams/%d/members?limit=50", t.ID), true, nil, &members); err != nil {
			return nil, err
		}
		for _, m := range members {
			if !seen[m.Login] {
				seen[m.Login] = true
				out = append(out, m.Login)
			}
		}
	}
	return out, nil
}

// EditRepoOption is the part of PATCH /repos/{owner}/{repo} ForgeSync uses
// (modules/structs/repo.go).
type EditRepoOption struct {
	Name     *string `json:"name,omitempty"`
	Archived *bool   `json:"archived,omitempty"`
}

// EditRepo changes a repository's settings.
func (c *Client) EditRepo(ctx context.Context, owner, repo string, opt EditRepoOption) error {
	return c.do(ctx, http.MethodPatch, repoPath(owner, repo), true, opt, nil)
}

// TransferRepo moves a repository to another owner. Forgejo does it at once
// when the caller may create repositories there, and otherwise leaves it
// pending the new owner's acceptance; callers check where it ended up.
func (c *Client) TransferRepo(ctx context.Context, owner, repo, newOwner string) error {
	return c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/transfer", true, map[string]string{"new_owner": newOwner}, nil)
}

// DeleteRepo deletes a repository. A repository that's already gone is not
// an error.
func (c *Client) DeleteRepo(ctx context.Context, owner, repo string) error {
	err := c.do(ctx, http.MethodDelete, repoPath(owner, repo), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// CreateOrgOption is the body of POST /admin/users/{username}/orgs
// (modules/structs/org.go).
type CreateOrgOption struct {
	UserName    string `json:"username"`
	FullName    string `json:"full_name,omitempty"`
	Description string `json:"description,omitempty"`
	Website     string `json:"website,omitempty"`
	Location    string `json:"location,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
}

// AdminCreateOrg creates an organization owned by the given user. Site
// admin only.
func (c *Client) AdminCreateOrg(ctx context.Context, owner string, opt CreateOrgOption) error {
	return c.do(ctx, http.MethodPost, "/api/v1/admin/users/"+url.PathEscape(owner)+"/orgs", true, opt, nil)
}

// Hook is a webhook as the admin API lists it (modules/structs/hook.go).
type Hook struct {
	ID     int64             `json:"id"`
	Type   string            `json:"type"`
	URL    string            `json:"url"`
	Config map[string]string `json:"config"`
	Events []string          `json:"events"`
	Active bool              `json:"active"`
}

// SystemHooks lists the instance's system and default webhooks. Site admin
// only.
func (c *Client) SystemHooks(ctx context.Context) ([]Hook, error) {
	var hooks []Hook
	err := c.do(ctx, http.MethodGet, "/api/v1/admin/hooks?limit=50", true, nil, &hooks)
	return hooks, err
}

// CreateSystemHook adds a system webhook: it fires for every repository.
func (c *Client) CreateSystemHook(ctx context.Context, url, secret string, events []string) (Hook, error) {
	var h Hook
	err := c.do(ctx, http.MethodPost, "/api/v1/admin/hooks", true, map[string]any{
		"type": "forgejo", "active": true, "events": events, "branch_filter": "*",
		"config": map[string]string{"url": url, "content_type": "json", "secret": secret,
			// Without this it's a "default" webhook, copied into new repositories.
			"is_system_webhook": "true"},
	}, &h)
	return h, err
}

// DeleteSystemHook removes a system or default webhook.
func (c *Client) DeleteSystemHook(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/admin/hooks/%d", id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Issue is the part of a Forgejo issue ForgeSync replicates.
type Issue struct {
	ID          int64     `json:"id"`
	Number      int64     `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"` // open or closed
	User        User      `json:"user"`
	Created     time.Time `json:"created_at"`
	PullRequest *struct{} `json:"pull_request"` // set for pull requests
	Labels      []Label   `json:"labels"`
	Milestone   *struct {
		ID int64 `json:"id"`
	} `json:"milestone"`
	Assignees []User `json:"assignees"`
}

// IssueComment is a comment on an issue or pull request.
type IssueComment struct {
	ID       int64     `json:"id"`
	IssueURL string    `json:"issue_url"` // .../issues/<number>
	PRURL    string    `json:"pull_request_url"`
	User     User      `json:"user"`
	Body     string    `json:"body"`
	Created  time.Time `json:"created_at"`
}

// Number is the number of the issue or pull request the comment belongs to
// (0 if neither URL says). On a pull request Forgejo leaves issue_url empty
// and fills pull_request_url instead, so both are read.
func (c IssueComment) Number() int64 {
	if n := numberIn(c.IssueURL); n != 0 {
		return n
	}
	return numberIn(c.PRURL)
}

func numberIn(url string) int64 {
	i := strings.LastIndex(url, "/")
	if i < 0 {
		return 0
	}
	n, _ := strconv.ParseInt(url[i+1:], 10, 64)
	return n
}

// ListIssues returns one page of a repository's issues (not pull requests),
// open and closed.
func (c *Client) ListIssues(ctx context.Context, owner, repo string, page, limit int) ([]Issue, error) {
	return c.listIssues(ctx, owner, repo, "issues", page, limit)
}

// ListIssuesAndPulls returns them with the pull requests among them, which
// carry their own issue id and are marked by PullRequest being set.
func (c *Client) ListIssuesAndPulls(ctx context.Context, owner, repo string, page, limit int) ([]Issue, error) {
	return c.listIssues(ctx, owner, repo, "all", page, limit)
}

func (c *Client) listIssues(ctx context.Context, owner, repo, kind string, page, limit int) ([]Issue, error) {
	var out []Issue
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues?state=all&type=%s&page=%d&limit=%d",
		repoPath(owner, repo), kind, page, limit), true, nil, &out)
	return out, err
}

// Issue returns one issue or pull request by number; found is false if
// it's gone. A pull request has its own id here, the one the issue
// listing gives it, which is not the pull request's own.
func (c *Client) Issue(ctx context.Context, owner, repo string, number int64) (Issue, bool, error) {
	var is Issue
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number), true, nil, &is)
	if isNotFound(err) {
		return Issue{}, false, nil
	}
	return is, err == nil, err
}

// ListPulls returns one page of a repository's pull requests, open and
// closed. It's read for what the issue listing doesn't carry: which
// branches each one is between, and whether it has been merged.
func (c *Client) ListPulls(ctx context.Context, owner, repo string, page, limit int) ([]PullRequest, error) {
	var out []PullRequest
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls?state=all&page=%d&limit=%d",
		repoPath(owner, repo), page, limit), true, nil, &out)
	return out, err
}

// ListRepoComments returns one page of the comments on all of a repository's
// issues and pull requests, oldest first.
func (c *Client) ListRepoComments(ctx context.Context, owner, repo string, page, limit int) ([]IssueComment, error) {
	var out []IssueComment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/comments?page=%d&limit=%d", repoPath(owner, repo), page, limit), true, nil, &out)
	return out, err
}

// CreateIssue opens an issue (already closed if closed is set) with these
// labels, milestone (0: none) and assignees.
func (c *Client) CreateIssue(ctx context.Context, owner, repo, title, body string, closed bool, labels []int64, milestone int64,
	assignees []string) (Issue, error) {
	var is Issue
	opt := map[string]any{"title": title, "body": body, "closed": closed}
	if len(labels) > 0 {
		opt["labels"] = labels
	}
	if milestone != 0 {
		opt["milestone"] = milestone
	}
	if len(assignees) > 0 {
		opt["assignees"] = assignees
	}
	err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/issues", true, opt, &is)
	return is, err
}

// SetIssueMilestone sets an issue's milestone (0: none).
func (c *Client) SetIssueMilestone(ctx context.Context, owner, repo string, number, milestone int64) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number), true,
		map[string]int64{"milestone": milestone}, nil)
}

// SetIssueAssignees sets an issue's assignees to exactly these logins; an
// empty list clears them. Forgejo refuses a user without write access to
// the repository, and clears the old assignees before adding the new ones,
// so callers check first (see ListAssignees).
func (c *Client) SetIssueAssignees(ctx context.Context, owner, repo string, number int64, logins []string) error {
	if logins == nil {
		logins = []string{}
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number), true,
		map[string]any{"assignees": logins}, nil)
}

// ListAssignees returns the users who can be assigned issues in the
// repository: those with write access.
func (c *Client) ListAssignees(ctx context.Context, owner, repo string) ([]User, error) {
	var out []User
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/assignees", true, nil, &out)
	return out, err
}

// ReplaceIssueLabels sets an issue's labels to exactly these.
func (c *Client) ReplaceIssueLabels(ctx context.Context, owner, repo string, number int64, labels []int64) error {
	if labels == nil {
		labels = []int64{}
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("%s/issues/%d/labels", repoPath(owner, repo), number), true,
		map[string]any{"labels": labels}, nil)
}

// EditIssue changes an issue's title, body or state; nil leaves a field.
func (c *Client) EditIssue(ctx context.Context, owner, repo string, number int64, title, body, state *string) error {
	opt := map[string]any{}
	if title != nil {
		opt["title"] = *title
	}
	if body != nil {
		opt["body"] = *body
	}
	if state != nil {
		opt["state"] = *state
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number), true, opt, nil)
}

// DeleteIssue deletes an issue (repository admins only). Already gone is
// not an error.
func (c *Client) DeleteIssue(ctx context.Context, owner, repo string, number int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/%d", repoPath(owner, repo), number), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// CreateIssueComment adds a comment and returns it.
func (c *Client) CreateIssueComment(ctx context.Context, owner, repo string, number int64, body string) (IssueComment, error) {
	var cm IssueComment
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repoPath(owner, repo), number), true,
		map[string]string{"body": body}, &cm)
	return cm, err
}

// EditIssueComment changes a comment's body.
func (c *Client) EditIssueComment(ctx context.Context, owner, repo string, id int64, body string) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/comments/%d", repoPath(owner, repo), id), true,
		map[string]string{"body": body}, nil)
}

// DeleteIssueComment deletes a comment. Already gone is not an error.
func (c *Client) DeleteIssueComment(ctx context.Context, owner, repo string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/comments/%d", repoPath(owner, repo), id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Reaction is one person's reaction to an issue or a comment
// (modules/structs/issue_reaction.go).
type Reaction struct {
	User    User      `json:"user"`
	Content string    `json:"content"`
	Created time.Time `json:"created_at"`
}

// IssueReactions returns one page of an issue's reactions.
func (c *Client) IssueReactions(ctx context.Context, owner, repo string, number int64, page, limit int) ([]Reaction, error) {
	return c.reactions(ctx, fmt.Sprintf("%s/issues/%d/reactions", repoPath(owner, repo), number), page, limit)
}

// CommentReactions returns one page of a comment's reactions.
func (c *Client) CommentReactions(ctx context.Context, owner, repo string, id int64, page, limit int) ([]Reaction, error) {
	return c.reactions(ctx, fmt.Sprintf("%s/issues/comments/%d/reactions", repoPath(owner, repo), id), page, limit)
}

func (c *Client) reactions(ctx context.Context, path string, page, limit int) ([]Reaction, error) {
	var out []Reaction
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?page=%d&limit=%d", path, page, limit), true, nil, &out)
	return out, err
}

// AddIssueReaction reacts to an issue as the client's user (Sudo picks who).
// Reacting again is not an error.
func (c *Client) AddIssueReaction(ctx context.Context, owner, repo string, number int64, content string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/reactions", repoPath(owner, repo), number), true,
		map[string]string{"content": content}, nil)
}

// RemoveIssueReaction takes that reaction away again.
func (c *Client) RemoveIssueReaction(ctx context.Context, owner, repo string, number int64, content string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/%d/reactions", repoPath(owner, repo), number), true,
		map[string]string{"content": content}, nil)
}

// AddCommentReaction reacts to a comment as the client's user.
func (c *Client) AddCommentReaction(ctx context.Context, owner, repo string, id int64, content string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/comments/%d/reactions", repoPath(owner, repo), id), true,
		map[string]string{"content": content}, nil)
}

// RemoveCommentReaction takes that reaction away again.
func (c *Client) RemoveCommentReaction(ctx context.Context, owner, repo string, id int64, content string) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/comments/%d/reactions", repoPath(owner, repo), id), true,
		map[string]string{"content": content}, nil)
}

// Label is a repository label (modules/structs/issue_label.go). Color comes
// back without the leading #.
type Label struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Exclusive   bool   `json:"exclusive"`
	IsArchived  bool   `json:"is_archived"`
}

// ListLabels returns one page of a repository's own labels (not its
// organization's).
func (c *Client) ListLabels(ctx context.Context, owner, repo string, page, limit int) ([]Label, error) {
	var out []Label
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/labels?page=%d&limit=%d", repoPath(owner, repo), page, limit), true, nil, &out)
	return out, err
}

// CreateLabel adds a label.
func (c *Client) CreateLabel(ctx context.Context, owner, repo string, l Label) (Label, error) {
	var out Label
	err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/labels", true, map[string]any{
		"name": l.Name, "color": "#" + strings.TrimPrefix(l.Color, "#"), "description": l.Description,
		"exclusive": l.Exclusive, "is_archived": l.IsArchived}, &out)
	return out, err
}

// EditLabel changes the given fields of a label (name, color, description,
// exclusive, is_archived).
func (c *Client) EditLabel(ctx context.Context, owner, repo string, id int64, fields map[string]any) error {
	if col, ok := fields["color"].(string); ok {
		fields["color"] = "#" + strings.TrimPrefix(col, "#")
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/labels/%d", repoPath(owner, repo), id), true, fields, nil)
}

// DeleteLabel removes a label. Already gone is not an error.
func (c *Client) DeleteLabel(ctx context.Context, owner, repo string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/labels/%d", repoPath(owner, repo), id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Milestone is a repository milestone (modules/structs/issue_milestone.go).
type Milestone struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	State       string     `json:"state"` // open or closed
	Deadline    *time.Time `json:"due_on"`
}

// ListMilestones returns one page of a repository's milestones, open and
// closed.
func (c *Client) ListMilestones(ctx context.Context, owner, repo string, page, limit int) ([]Milestone, error) {
	var out []Milestone
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/milestones?state=all&page=%d&limit=%d", repoPath(owner, repo), page, limit), true, nil, &out)
	return out, err
}

// CreateMilestone adds a milestone.
func (c *Client) CreateMilestone(ctx context.Context, owner, repo string, m Milestone) (Milestone, error) {
	var out Milestone
	opt := map[string]any{"title": m.Title, "description": m.Description, "state": m.State}
	if m.Deadline != nil {
		opt["due_on"] = m.Deadline
	}
	err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/milestones", true, opt, &out)
	return out, err
}

// EditMilestone changes the given fields (title, description, state,
// due_on). Forgejo can set a due date but not clear one.
func (c *Client) EditMilestone(ctx context.Context, owner, repo string, id int64, fields map[string]any) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/milestones/%d", repoPath(owner, repo), id), true, fields, nil)
}

// DeleteMilestone removes a milestone. Already gone is not an error.
func (c *Client) DeleteMilestone(ctx context.Context, owner, repo string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/milestones/%d", repoPath(owner, repo), id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}
