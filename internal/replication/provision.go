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
}

// blocked is a reason a missing repository can't be created on a replica
// yet. It's shown to people; it isn't an error in ForgeSync.
type blocked string

func (b blocked) Error() string { return string(b) }

// createOnReplica creates the repository on a replica where it's missing,
// empty, so the next push fills it. The owner must be a SceneID user (who is
// created on the replica too, linked by their SceneID subject so their first
// login there finds the account) or an organization that already exists on
// the replica. It returns a blocked error when that isn't the case, and
// never changes an existing account.
func (e *Engine) createOnReplica(ctx context.Context, fullName string, primary, replica Node) error {
	if primary.API == nil || replica.API == nil {
		return blocked("the repository doesn't exist on " + replica.Name + "; create it there to start replicating")
	}
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return fmt.Errorf("bad repository name %q", fullName)
	}
	src, found, err := primary.API.GetRepo(ctx, owner, name)
	if err != nil {
		return err
	}
	if !found {
		return blocked("the repository doesn't exist on the primary " + primary.Name)
	}
	switch {
	case src.Mirror:
		return blocked("it's a pull mirror on " + primary.Name + "; ForgeSync doesn't create mirrors on other nodes")
	case src.Fork:
		return blocked("it's a fork on " + primary.Name + "; ForgeSync doesn't create forks on other nodes yet")
	}
	owner, name = src.Owner.Login, src.Name // the primary's spelling

	isOrg, err := primary.API.IsOrg(ctx, owner)
	if err != nil {
		return err
	}
	if isOrg {
		exists, err := replica.API.IsOrg(ctx, owner)
		if err != nil {
			return err
		}
		if !exists {
			return blocked(fmt.Sprintf("the owner, organization %s, doesn't exist on %s; ForgeSync doesn't create organizations yet", owner, replica.Name))
		}
	} else if err := e.ensureUser(ctx, owner, primary, replica); err != nil {
		return err
	}

	_, err = replica.API.AdminCreateRepo(ctx, owner, forgejo.CreateRepoOption{
		Name: name, Description: src.Description, Private: src.Private, Template: src.Template,
		DefaultBranch: src.DefaultBranch, ObjectFormatName: src.ObjectFormatName,
	})
	if forgejo.IsConflict(err) {
		return nil // created in the meantime
	}
	if err != nil {
		return err
	}
	e.log.Info("created repository on replica", "repository", fullName, "node", replica.Name)
	if err := e.store.Audit(ctx, "forgesync", "repo.created_on_node", fullName,
		map[string]any{"node": replica.Name, "primary": primary.Name, "private": src.Private}); err != nil {
		e.log.Error("writing audit log failed", "error", err)
	}
	return nil
}

// ensureUser makes sure the owner exists on the replica as the same account.
//
// Regular users all come from SceneID, so the owner has signed in there and
// is copied with the same SceneID subject; their first login on the replica
// finds the account. Local accounts are only admins (e.g. siteadmin), set
// up on every node, so a local owner is matched by name and never created.
//
// The checks that return blocked shouldn't fire in normal operation; they
// keep inconsistent data (a name held by a different account) from merging
// two people into one account.
func (e *Engine) ensureUser(ctx context.Context, login string, primary, replica Node) error {
	pu, found, err := primary.API.GetUser(ctx, login)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("owner %s not found on %s", login, primary.Name)
	}
	ru, onReplica, err := replica.API.GetUser(ctx, login)
	if err != nil {
		return err
	}
	if pu.SourceID == 0 { // a local admin account
		if !onReplica || ru.SourceID != 0 {
			return blocked(fmt.Sprintf("the owner %s is a local account on %s and there's no local account %s on %s; ForgeSync doesn't create local accounts",
				login, primary.Name, login, replica.Name))
		}
		return nil
	}
	if primary.SceneIDSourceID == 0 || replica.SceneIDSourceID == 0 {
		return blocked(fmt.Sprintf("the owner %s can't be created on %s: sceneid_source_id isn't configured for both %s and %s",
			login, replica.Name, primary.Name, replica.Name))
	}
	if pu.SourceID != primary.SceneIDSourceID || pu.LoginName == "" {
		return blocked(fmt.Sprintf("the owner %s on %s signs in through a login source other than SceneID", login, primary.Name))
	}
	if onReplica {
		if ru.SourceID != replica.SceneIDSourceID || ru.LoginName != pu.LoginName {
			return blocked(fmt.Sprintf("the name %s on %s belongs to a different account than on %s; ForgeSync won't merge them",
				login, replica.Name, primary.Name))
		}
		return nil
	}
	// The same person may already be on the replica under another name.
	same, err := replica.API.UsersByLoginName(ctx, replica.SceneIDSourceID, pu.LoginName)
	if err != nil {
		return err
	}
	if len(same) > 0 {
		return blocked(fmt.Sprintf("the SceneID user %s is %s on %s; ForgeSync doesn't rename accounts",
			login, same[0].Login, replica.Name))
	}
	no := false
	opt := forgejo.CreateUserOption{
		SourceID: replica.SceneIDSourceID, LoginName: pu.LoginName, Username: pu.Login,
		FullName: pu.FullName, Email: pu.Email, MustChangePassword: &no, Visibility: pu.Visibility,
	}
	if !pu.Created.IsZero() {
		opt.Created = &pu.Created
	}
	if _, err := replica.API.AdminCreateUser(ctx, opt); err != nil {
		if forgejo.IsConflict(err) { // e.g. the e-mail address belongs to another account there
			return blocked(fmt.Sprintf("%s refused to create the owner %s: %v", replica.Name, login, err))
		}
		return err
	}
	e.log.Info("created user on node", "user", login, "node", replica.Name)
	if err := e.store.Audit(ctx, "forgesync", "user.created_on_node", login,
		map[string]any{"node": replica.Name, "from": primary.Name}); err != nil {
		e.log.Error("writing audit log failed", "error", err)
	}
	return nil
}
