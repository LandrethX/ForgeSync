package replication

import (
	"context"
	"strings"

	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// Collaborators are the people a repository is shared with, and what each
// of them may do: read, write or admin. A login means the same person on
// every node, so "<login>:<permission>" identifies one wherever it was
// granted, and they merge member by member (internal/set) like reactions
// and reviews: granted anywhere is granted everywhere, taken away anywhere
// is taken away everywhere.
//
// Changing what someone may do reads as one member going and another
// arriving. Both land on the same node in one run, and the grant is
// written as an update rather than a removal and a grant, so nobody loses
// access in between.
//
// A member enters the base only once every node has it and leaves only
// once none has, so someone ForgeSync couldn't add -- a person it can't
// create on that node -- is tried again next run instead of their absence
// being read as the access being withdrawn. The owner and site admins
// aren't collaborators and never appear here.

// collaboratorsOn is what one node has, as members.
func (e *Engine) collaboratorsOn(ctx context.Context, n Node, owner, name string) (map[string]bool, error) {
	people, err := n.API.Collaborators(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, u := range people {
		perm, err := n.API.CollaboratorPermission(ctx, owner, name, u.Login)
		if err != nil {
			return nil, err
		}
		if perm == "" || perm == "none" {
			continue
		}
		out[u.Login+":"+perm] = true
	}
	return out, nil
}

// syncCollaborators brings one repository's collaborators together across
// the nodes that have it.
func (e *Engine) syncCollaborators(ctx context.Context, rec store.RepositoryRecord, primary Node, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	have := map[string]bool{} // which nodes took part
	at := map[string]map[string]bool{}
	unread := 0 // nodes that have the repository and could not be read
	for _, n := range e.order {
		node := e.nodes[n]
		if !e.hasRepo(rec, n) {
			continue
		}
		if !healthy[n] {
			unread++
			continue
		}
		on, err := e.collaboratorsOn(ctx, node, owner, name)
		if err != nil {
			e.log.Warn("collaborators: reading them failed", "repository", rec.FullName, "node", n, "error", err)
			unread++
			continue
		}
		at[n], have[n] = on, true
	}
	if len(at) < 2 {
		return // nothing to compare
	}
	base := set.From(rec.BaseCollaborators)
	plan := set.Decide(at, base)
	for _, n := range e.order {
		node, taking := e.nodes[n], map[string]bool{}
		for _, m := range plan.Add[n] {
			taking[loginOf(m)] = true
		}
		for _, m := range plan.Remove[n] {
			// Someone whose access is only changing is updated in the grant
			// below, so they don't lose it in between.
			if taking[loginOf(m)] {
				delete(at[n], m)
				continue
			}
			if err := node.API.RemoveCollaborator(ctx, owner, name, loginOf(m)); err != nil {
				e.log.Warn("collaborators: removing one failed; left for the next run", "repository", rec.FullName,
					"node", n, "who", loginOf(m), "error", err)
				continue
			}
			delete(at[n], m)
			e.log.Info("collaborator removed", "repository", rec.FullName, "node", n, "who", loginOf(m))
			e.audit(ctx, "repo.collaborator_removed", rec.FullName,
				map[string]any{"repository_id": rec.ID, "node": n, "who": loginOf(m)})
		}
		for _, m := range plan.Add[n] {
			who, perm := loginOf(m), permissionOf(m)
			if err := e.EnsureUser(ctx, who, primary.Name, n); err != nil {
				e.log.Info("collaborators: the person can't be created on the node; left for the next run",
					"repository", rec.FullName, "node", n, "who", who, "error", err)
				continue
			}
			if err := node.API.AddCollaborator(ctx, owner, name, who, perm); err != nil {
				e.log.Warn("collaborators: granting failed; left for the next run", "repository", rec.FullName,
					"node", n, "who", who, "permission", perm, "error", err)
				continue
			}
			at[n][m] = true
			e.log.Info("collaborator granted", "repository", rec.FullName, "node", n, "who", who, "permission", perm)
			e.audit(ctx, "repo.collaborator_granted", rec.FullName,
				map[string]any{"repository_id": rec.ID, "node": n, "who": who, "permission": perm})
		}
	}
	if now := settle(unread, rec.BaseCollaborators, at, base); now != rec.BaseCollaborators {
		if err := e.store.SetRepositoryCollaborators(ctx, rec.ID, now); err != nil {
			e.log.Error("collaborators: recording them failed", "repository", rec.FullName, "error", err)
		}
	}
}

func loginOf(member string) string {
	login, _, _ := strings.Cut(member, ":")
	return login
}

func permissionOf(member string) string {
	_, perm, _ := strings.Cut(member, ":")
	return perm
}

// hasRepo reports that the node is recorded as having the repository.
func (e *Engine) hasRepo(rec store.RepositoryRecord, node string) bool {
	for _, rp := range rec.Replicas {
		if rp.Node == node {
			return rp.Present
		}
	}
	return false
}
