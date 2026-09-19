package replication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
)

// Who ends up on the nodes, and who doesn't: the accounts here are the
// people who use the Forgejo nodes. ForgeSync's own administrators are
// administrators of the *controllers* -- their role comes from a SceneID
// claim and lives in the controller alone -- so nothing gives them an
// account on a node, and the local accounts that do exist there (a site
// admin, ForgeSync's service account) are never created or copied either.
//
// People come from SceneID: they get a Forgejo account on a node the
// first time they sign in there, and ForgeSync makes the same account on
// another node when something of theirs is copied to it. That leaves two
// gaps an administrator hits in practice.
//
// The first is someone who has signed in somewhere but has no account on
// the other nodes yet, because nothing of theirs has been copied: they
// can't be added as a collaborator there, and a repository transferred to
// them can't be created. ProvisionUser copies the account they have to
// the nodes that lack it.
//
// The second is someone who has never signed in at all -- a new member
// being set up before their first visit. CreateUser makes their account
// on every node from their SceneID subject, which is what links it to
// them when they do sign in (p01). It is not a way to make a local
// account: there's no password, the account signs in through SceneID like
// every other, and the subject is what ForgeSync keys people by.
//
// Both are careful in the same way as everything else here: an account
// that already matches is left alone, and anything that would merge two
// people into one account is refused for that node, with the reason, and
// nothing is written there.

// NewUser is an account to create on every node.
type NewUser struct {
	// Login is the username, Subject the SceneID `sub` that identifies the
	// person. Both are theirs on every node.
	Login, Subject string
	FullName       string
	Email          string
	// Home is the node that counts as where they registered, so they get
	// it as their primary site. The others are recorded as accounts
	// ForgeSync created, which are never taken for a registration.
	Home string
}

// CreateUser makes the account on every node. It returns the nodes it was
// created on and, per node, why it wasn't where it wasn't.
func (e *Engine) CreateUser(ctx context.Context, u NewUser) ([]string, map[string]string, error) {
	u.Login, u.Subject = strings.TrimSpace(u.Login), strings.TrimSpace(u.Subject)
	switch {
	case u.Login == "":
		return nil, nil, fmt.Errorf("the username is missing")
	case u.Subject == "":
		return nil, nil, fmt.Errorf("the SceneID subject is missing")
	}
	if _, ok := e.nodes[u.Home]; u.Home != "" && !ok {
		return nil, nil, fmt.Errorf("no node called %s", u.Home)
	}
	created := []string{}
	refused := map[string]string{}
	// The home node first, so an account that ForgeSync then fails to
	// create elsewhere still leaves the person a primary site.
	for _, name := range e.orderFrom(u.Home) {
		n := e.nodes[name]
		if n.API == nil {
			refused[name] = "ForgeSync has no API token for this node"
			continue
		}
		made, err := e.createUserOn(ctx, u, n)
		var why blocked
		switch {
		case errors.As(err, &why):
			refused[name] = why.Error()
			continue
		case err != nil:
			refused[name] = err.Error()
			continue
		case !made:
			continue // already there, and it's the same person
		}
		created = append(created, name)
		if name != u.Home {
			// Only the home node counts as where they registered.
			if err := e.store.NoteCreatedAccount(ctx, name, u.Login); err != nil {
				e.log.Error("users: recording the created account failed", "user", u.Login, "node", name, "error", err)
			}
		}
		e.log.Info("created user on node", "user", u.Login, "node", name)
		e.audit(ctx, "user.created_on_node", u.Login, map[string]any{"node": name, "by": "an administrator"})
	}
	return created, refused, nil
}

// createUserOn makes one account, or leaves an account that's already the
// same person alone. It refuses anything that would join two people.
func (e *Engine) createUserOn(ctx context.Context, u NewUser, n Node) (bool, error) {
	if n.SceneIDSourceID == 0 {
		return false, blocked("sceneid_source_id isn't configured for " + n.Name)
	}
	there, found, err := n.API.GetUser(ctx, u.Login)
	if err != nil {
		return false, err
	}
	if found {
		switch {
		case there.SourceID == 0:
			// A local account: the site admin, or ForgeSync's own service
			// account. Those belong to whoever runs the nodes, and
			// ForgeSync's administrators are administrators of the
			// controllers, not of the nodes -- neither is copied here.
			return false, blocked(fmt.Sprintf("%s is a local account on %s (a site admin or ForgeSync's own); those aren't ForgeSync's to create",
				u.Login, n.Name))
		case there.SourceID != n.SceneIDSourceID || there.LoginName != u.Subject:
			return false, blocked(fmt.Sprintf("the name %s already belongs to a different account on %s", u.Login, n.Name))
		}
		return false, nil // the same person is already there
	}
	// The same subject may be there under another name.
	same, err := n.API.UsersByLoginName(ctx, n.SceneIDSourceID, u.Subject)
	if err != nil {
		return false, err
	}
	if len(same) > 0 {
		return false, blocked(fmt.Sprintf("that SceneID subject is %s on %s; ForgeSync doesn't rename accounts", same[0].Login, n.Name))
	}
	no := false
	email := u.Email
	if email == "" {
		// Forgejo insists on one. It's replaced by SceneID's at the first
		// sign-in, which is when the account becomes theirs in earnest.
		email = u.Login + "@users.noreply.invalid"
	}
	_, err = n.API.AdminCreateUser(ctx, forgejo.CreateUserOption{
		SourceID: n.SceneIDSourceID, LoginName: u.Subject, Username: u.Login,
		FullName: u.FullName, Email: email, MustChangePassword: &no,
	})
	if forgejo.IsConflict(err) {
		return false, blocked(fmt.Sprintf("%s refused it: %v", n.Name, err))
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ProvisionUser copies an account someone already has to the nodes that
// haven't got it, without waiting for one of their repositories to be
// replicated there.
func (e *Engine) ProvisionUser(ctx context.Context, login string) ([]string, map[string]string, error) {
	login = strings.TrimSpace(login)
	if login == "" {
		return nil, nil, fmt.Errorf("the username is missing")
	}
	var from *Node
	missing := []string{}
	refused := map[string]string{}
	for _, name := range e.order {
		n := e.nodes[name]
		if n.API == nil {
			refused[name] = "ForgeSync has no API token for this node"
			continue
		}
		u, found, err := n.API.GetUser(ctx, login)
		switch {
		case err != nil:
			refused[name] = err.Error()
		case found && u.SourceID != 0 && from == nil:
			node := n
			from = &node
		case !found:
			missing = append(missing, name)
		}
	}
	if from == nil {
		return nil, refused, fmt.Errorf("no node has a SceneID account called %s", login)
	}
	created := []string{}
	for _, name := range missing {
		var why blocked
		switch err := e.ensureUser(ctx, login, *from, e.nodes[name]); {
		case errors.As(err, &why):
			refused[name] = why.Error()
		case err != nil:
			refused[name] = err.Error()
		default:
			created = append(created, name)
		}
	}
	sort.Strings(created)
	return created, refused, nil
}

// orderFrom is the nodes with one of them first.
func (e *Engine) orderFrom(first string) []string {
	out := make([]string, 0, len(e.order))
	if _, ok := e.nodes[first]; ok {
		out = append(out, first)
	}
	for _, n := range e.order {
		if n != first {
			out = append(out, n)
		}
	}
	return out
}
