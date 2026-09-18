package webhook

import (
	"context"
	"log/slog"
)

// Lookup finds repositories in ForgeSync's inventory.
type Lookup interface {
	RepositoryIDByName(ctx context.Context, fullName string) (string, error)
	PreviousName(ctx context.Context, node, fullName string) (string, error)
}

// RepoScanner looks at one repository on every node.
type RepoScanner interface {
	ScanRepo(ctx context.Context, fullName string)
}

// Replicator replicates one repository.
type Replicator interface {
	Trigger(ctx context.Context, id string) (bool, error)
}

// IssueReplicator replicates one repository's issues.
type IssueReplicator interface {
	Trigger(ctx context.Context, id string)
}

// RepoDispatcher turns a reported change into work on that one repository.
type RepoDispatcher struct {
	Store   Lookup
	Scanner RepoScanner
	// Replicator is nil when replication is off; then only the inventory is
	// refreshed.
	Replicator Replicator
	// Issues is nil when issue replication is off.
	Issues IssueReplicator
	// Assign applies renames and the primary rules after the inventory
	// changed (inventory.AssignPrimaries).
	Assign func(ctx context.Context) error
	Log    *slog.Logger
}

// Changed handles one change. A push to a repository ForgeSync knows goes
// straight to replication. A repository it doesn't know yet, or one created
// or deleted, is looked at on every node first, so the primary, rename and
// deletion rules see it.
func (d *RepoDispatcher) Changed(ctx context.Context, c Change) {
	id, err := d.Store.RepositoryIDByName(ctx, c.Repository)
	if err != nil {
		d.Log.Error("webhook: looking up the repository failed", "repository", c.Repository, "error", err)
		return
	}
	if IssueEvents[c.Event] {
		// Issues of a repository ForgeSync doesn't know yet are picked up
		// once it does (the next scan, or its first push).
		if id != "" && d.Issues != nil {
			d.Issues.Trigger(ctx, id)
		}
		return
	}
	if id == "" || c.Event == "repository" {
		d.Scanner.ScanRepo(ctx, c.Repository)
		// Renames send no webhook: a push under a new name can be the first
		// sign. If the node lists the same repository under another name,
		// look at that name too, so the rename is detected instead of the
		// new name being copied next to the old one.
		if prev, err := d.Store.PreviousName(ctx, c.Node, c.Repository); err != nil {
			d.Log.Error("webhook: checking for a rename failed", "repository", c.Repository, "error", err)
		} else if prev != "" {
			d.Log.Info("webhook: repository has another name in the inventory; checking it too", "repository", c.Repository, "previous", prev)
			d.Scanner.ScanRepo(ctx, prev)
		}
		if d.Assign != nil {
			if err := d.Assign(ctx); err != nil {
				d.Log.Error("webhook: assigning primaries failed", "error", err)
			}
		}
		if id, err = d.Store.RepositoryIDByName(ctx, c.Repository); err != nil || id == "" {
			return
		}
	}
	if d.Replicator == nil {
		return
	}
	if _, err := d.Replicator.Trigger(ctx, id); err != nil {
		d.Log.Info("webhook: not replicating", "repository", c.Repository, "reason", err)
	}
}
