package replication

import (
	"context"
	"fmt"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
)

// NodeAPI is the part of the Forgejo REST API that creating a missing
// repository on a replica needs. *forgejo.Client implements it.
type NodeAPI interface {
	GetUser(ctx context.Context, login string) (forgejo.User, bool, error)
	IsOrg(ctx context.Context, name string) (bool, error)
	GetRepo(ctx context.Context, owner, name string) (forgejo.Repository, bool, error)
	UsersByLoginName(ctx context.Context, sourceID int64, loginName string) ([]forgejo.User, error)
	AdminCreateUser(ctx context.Context, opt forgejo.CreateUserOption) (forgejo.User, error)
	AdminCreateRepo(ctx context.Context, owner string, opt forgejo.CreateRepoOption) (forgejo.Repository, error)
	GetOrg(ctx context.Context, name string) (forgejo.Org, bool, error)
	EditOrg(ctx context.Context, name string, fields map[string]any) error
	OrgTeams(ctx context.Context, org string) ([]forgejo.Team, error)
	CreateTeam(ctx context.Context, org string, t forgejo.Team) (forgejo.Team, error)
	DeleteTeam(ctx context.Context, id int64) error
	TeamMembers(ctx context.Context, id int64) ([]forgejo.User, error)
	AddTeamMember(ctx context.Context, id int64, login string) error
	RemoveTeamMember(ctx context.Context, id int64, login string) error
	Collaborators(ctx context.Context, owner, repo string) ([]forgejo.User, error)
	CollaboratorPermission(ctx context.Context, owner, repo, login string) (string, error)
	AddCollaborator(ctx context.Context, owner, repo, login, permission string) error
	RemoveCollaborator(ctx context.Context, owner, repo, login string) error

	// For resolving conflicts (handoff.go).
	SetDefaultBranch(ctx context.Context, owner, repo, branch string) error
	CreatePullRequest(ctx context.Context, owner, repo string, opt forgejo.CreatePullRequestOption) (forgejo.PullRequest, error)
	GetPullRequest(ctx context.Context, owner, repo string, number int64) (forgejo.PullRequest, bool, error)
	Comment(ctx context.Context, owner, repo string, number int64, body string) error
	OrgOwners(ctx context.Context, org string) ([]string, error)

	// For repositories deleted on their primary (deletion.go).
	EditRepo(ctx context.Context, owner, repo string, opt forgejo.EditRepoOption) error
	TransferRepo(ctx context.Context, owner, repo, newOwner string) error
	DeleteRepo(ctx context.Context, owner, repo string) error
	AdminCreateOrg(ctx context.Context, owner string, opt forgejo.CreateOrgOption) error
}

// blocked is a reason a missing repository can't be created on a replica
// yet. It's shown to people; it isn't an error in ForgeSync.
type blocked string

func (b blocked) Error() string { return string(b) }

// createOnReplica creates the repository on node to, where it's missing,
// empty, so the next push from node from fills it. The owner must be a SceneID user (who is
// created on the replica too, linked by their SceneID subject so their first
// login there finds the account) or an organization that already exists on
// the to. It returns a blocked error when that isn't the case, and
// never changes an existing account.
func (e *Engine) createOnReplica(ctx context.Context, fullName string, from, to Node) error {
	if from.API == nil || to.API == nil {
		return blocked("the repository doesn't exist on " + to.Name + "; create it there to start replicating")
	}
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return fmt.Errorf("bad repository name %q", fullName)
	}
	src, found, err := from.API.GetRepo(ctx, owner, name)
	if err != nil {
		return err
	}
	if !found {
		return blocked("the repository doesn't exist on " + from.Name)
	}
	switch {
	case src.Mirror:
		return blocked("it's a pull mirror on " + from.Name + "; ForgeSync doesn't create mirrors on other nodes")
	case src.Fork:
		return blocked("it's a fork on " + from.Name + "; ForgeSync doesn't create forks on other nodes yet")
	}
	owner, name = src.Owner.Login, src.Name // the source node's spelling

	isOrg, err := from.API.IsOrg(ctx, owner)
	if err != nil {
		return err
	}
	if isOrg {
		exists, err := to.API.IsOrg(ctx, owner)
		if err != nil {
			return err
		}
		if !exists {
			return blocked(fmt.Sprintf("the owner, organization %s, doesn't exist on %s; ForgeSync doesn't create organizations yet", owner, to.Name))
		}
	} else if err := e.ensureUser(ctx, owner, from, to); err != nil {
		return err
	}

	_, err = to.API.AdminCreateRepo(ctx, owner, forgejo.CreateRepoOption{
		Name: name, Description: src.Description, Private: src.Private, Template: src.Template,
		DefaultBranch: src.DefaultBranch, ObjectFormatName: src.ObjectFormatName,
	})
	if forgejo.IsConflict(err) {
		return nil // created in the meantime
	}
	if err != nil {
		return err
	}
	e.log.Info("created repository on node", "repository", fullName, "node", to.Name)
	if err := e.store.Audit(ctx, "forgesync", "repo.created_on_node", fullName,
		map[string]any{"node": to.Name, "from": from.Name, "private": src.Private}); err != nil {
		e.log.Error("writing audit log failed", "error", err)
	}
	return nil
}

// ensureUser makes sure the owner on node from exists on node to as the same
// account.
//
// Regular users all come from SceneID, so the owner has signed in there and
// is copied with the same SceneID subject; their first login on the replica
// finds the account. Local accounts are only admins (e.g. siteadmin), set
// up on every node, so a local owner is matched by name and never created.
//
// The checks that return blocked shouldn't fire in normal operation; they
// keep inconsistent data (a name held by a different account) from merging
// two people into one account.
func (e *Engine) ensureUser(ctx context.Context, login string, from, to Node) error {
	pu, found, err := from.API.GetUser(ctx, login)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("owner %s not found on %s", login, from.Name)
	}
	ru, onReplica, err := to.API.GetUser(ctx, login)
	if err != nil {
		return err
	}
	if pu.SourceID == 0 { // a local admin account
		if !onReplica || ru.SourceID != 0 {
			return blocked(fmt.Sprintf("the owner %s is a local account on %s and there's no local account %s on %s; ForgeSync doesn't create local accounts",
				login, from.Name, login, to.Name))
		}
		return nil
	}
	if from.SceneIDSourceID == 0 || to.SceneIDSourceID == 0 {
		return blocked(fmt.Sprintf("the owner %s can't be created on %s: sceneid_source_id isn't configured for both %s and %s",
			login, to.Name, from.Name, to.Name))
	}
	if pu.SourceID != from.SceneIDSourceID || pu.LoginName == "" {
		return blocked(fmt.Sprintf("the owner %s on %s signs in through a login source other than SceneID", login, from.Name))
	}
	if onReplica {
		if ru.SourceID != to.SceneIDSourceID || ru.LoginName != pu.LoginName {
			return blocked(fmt.Sprintf("the name %s on %s belongs to a different account than on %s; ForgeSync won't merge them",
				login, to.Name, from.Name))
		}
		return nil
	}
	// The same person may already be on the replica under another name.
	same, err := to.API.UsersByLoginName(ctx, to.SceneIDSourceID, pu.LoginName)
	if err != nil {
		return err
	}
	if len(same) > 0 {
		return blocked(fmt.Sprintf("the SceneID user %s is %s on %s; ForgeSync doesn't rename accounts",
			login, same[0].Login, to.Name))
	}
	no := false
	opt := forgejo.CreateUserOption{
		SourceID: to.SceneIDSourceID, LoginName: pu.LoginName, Username: pu.Login,
		FullName: pu.FullName, Email: pu.Email, MustChangePassword: &no, Visibility: pu.Visibility,
	}
	if _, err := to.API.AdminCreateUser(ctx, opt); err != nil {
		if forgejo.IsConflict(err) { // e.g. the e-mail address belongs to another account there
			return blocked(fmt.Sprintf("%s refused to create the owner %s: %v", to.Name, login, err))
		}
		return err
	}
	// Never count this account as where the user registered.
	if err := e.store.NoteCreatedAccount(ctx, to.Name, pu.Login); err != nil {
		return err
	}
	e.log.Info("created user on node", "user", login, "node", to.Name)
	if err := e.store.Audit(ctx, "forgesync", "user.created_on_node", login,
		map[string]any{"node": to.Name, "from": from.Name}); err != nil {
		e.log.Error("writing audit log failed", "error", err)
	}
	return nil
}

// EnsureUser makes the account login on node from exist on node to as the
// same account, as for repository owners (e.g. an issue's author before
// ForgeSync copies the issue as them).
func (e *Engine) EnsureUser(ctx context.Context, login, from, to string) error {
	f, ok1 := e.nodes[from]
	t, ok2 := e.nodes[to]
	if !ok1 || !ok2 || f.API == nil || t.API == nil {
		return fmt.Errorf("no API for %s or %s", from, to)
	}
	return e.ensureUser(ctx, login, f, t)
}
