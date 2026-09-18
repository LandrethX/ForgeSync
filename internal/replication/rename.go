package replication

import (
	"context"
	"fmt"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// Renames and transfers on the primary.
//
// store.DetectRenames spots them by Forgejo's repository id and gives the
// repository its new name; each replica's copy keeps its old name until
// renameOn applies the change there (Replica.FullName says which). That runs
// before replication for the node, so the copy is renamed instead of a new
// one being created next to it. A transfer to another owner also moves the
// copy, creating the owner there as for a new repository. The steps resume
// where an earlier attempt stopped.

// pendingRename returns the name of a replica's copy if it still has to be
// renamed to the repository's name.
func pendingRename(rec store.RepositoryRecord, node string) string {
	for _, r := range rec.Replicas {
		if r.Node == node && r.Present && r.FullName != "" && !strings.EqualFold(r.FullName, rec.FullName) {
			return r.FullName
		}
	}
	return ""
}

// renameOn gives the copy on a replica the repository's new name (and owner).
func (e *Engine) renameOn(ctx context.Context, rec store.RepositoryRecord, primary, n Node, oldFull string) error {
	if n.API == nil {
		return blocked(fmt.Sprintf("renamed on the primary; ForgeSync can't reach %s's API to rename %s", n.Name, oldFull))
	}
	oldOwner, oldName, _ := strings.Cut(oldFull, "/")
	newOwner, newName, _ := strings.Cut(rec.FullName, "/")
	sameOwner := strings.EqualFold(oldOwner, newOwner)

	_, hasOld, err := n.API.GetRepo(ctx, oldOwner, oldName)
	if err != nil {
		return err
	}
	_, hasNew, err := n.API.GetRepo(ctx, newOwner, newName)
	if err != nil {
		return err
	}
	// Renamed but not moved yet, from an earlier attempt.
	hasMid := false
	if !sameOwner && !hasOld {
		if _, hasMid, err = n.API.GetRepo(ctx, oldOwner, newName); err != nil {
			return err
		}
	}
	switch {
	case hasOld && hasNew:
		return blocked(fmt.Sprintf("renamed on the primary to %s, but %s already has a different repository by that name; "+
			"its copy stays as %s", rec.FullName, n.Name, oldFull))
	case hasNew:
		return nil // done earlier; the next scan sees it
	case !hasOld && !hasMid:
		return nil // no copy to rename; replication creates one
	}

	if !sameOwner {
		isOrg, err := primary.API.IsOrg(ctx, newOwner)
		if err != nil {
			return err
		}
		if isOrg {
			if ok, err := n.API.IsOrg(ctx, newOwner); err != nil {
				return err
			} else if !ok {
				return blocked(fmt.Sprintf("moved on the primary to organization %s, which doesn't exist on %s; "+
					"ForgeSync doesn't create organizations yet", newOwner, n.Name))
			}
		} else if err := e.ensureUser(ctx, newOwner, primary, n); err != nil {
			return err
		}
	}
	if hasOld && !strings.EqualFold(oldName, newName) {
		name := newName
		if err := n.API.EditRepo(ctx, oldOwner, oldName, forgejo.EditRepoOption{Name: &name}); err != nil {
			return fmt.Errorf("renaming %s: %w", oldFull, err)
		}
	}
	if !sameOwner {
		if err := n.API.TransferRepo(ctx, oldOwner, newName, newOwner); err != nil {
			return fmt.Errorf("moving it to %s: %w", newOwner, err)
		}
		if _, there, err := n.API.GetRepo(ctx, newOwner, newName); err != nil {
			return err
		} else if !there {
			return fmt.Errorf("moving it to %s is waiting for acceptance", newOwner)
		}
	}
	e.log.Info("copy renamed as on the primary", "repository", rec.FullName, "node", n.Name, "from", oldFull)
	e.audit(ctx, "repo.renamed_on_node", rec.FullName, map[string]any{
		"repository_id": rec.ID, "node": n.Name, "from": oldFull, "to": rec.FullName})
	return nil
}
