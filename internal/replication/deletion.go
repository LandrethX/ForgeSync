package replication

import (
	"context"
	"fmt"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// Repositories deleted on their primary.
//
// Deleting a repository on its primary is the owner's decision, but ForgeSync
// never deletes the other copies outright: on each other node it renames the
// copy to <owner>--<name>--<time>, moves it into the private archive
// organization (Options.ArchiveOrg, created on demand, owned by the service
// account) and marks it archived. That keeps everything, including issues,
// the wiki and commits that were never replicated. After BackupFor, the
// archived copies are deleted and ForgeSync forgets the repository.
//
// It only acts when both the primary's Git server says the repository is gone
// and the inventory confirms the primary had it and no longer does. A
// repository the primary never had is copied there instead (seedPrimary). A
// copy deleted on a replica is recreated from the primary, as before. If the
// repository is created again on the primary, it's a normal repository again;
// its archives stay until they expire.

// StateArchived: deleted on the primary and archived on this replica.
const StateArchived = "archived"

// primaryHad reports whether the primary had the repository, and whether the
// inventory confirms it's gone there.
func primaryHad(rec store.RepositoryRecord, primary string) (had, gone bool) {
	if rec.DeletedAt != nil {
		return true, true
	}
	for _, r := range rec.Replicas {
		if r.Node == primary && r.LastSeenAt != nil {
			return true, !r.Present
		}
	}
	return false, false
}

// deletedOnPrimary archives the copies on the replicas. It returns whether
// every replica was dealt with.
func (e *Engine) deletedOnPrimary(ctx context.Context, rec store.RepositoryRecord, primary Node, replicas []string,
	healthy map[string]bool, record func(node, state, detail string, updated int, refs map[string]string)) (bool, error) {

	now := e.now().UTC()
	if rec.DeletedAt == nil {
		if err := e.store.MarkRepositoryDeleted(ctx, rec.ID, now); err != nil {
			return false, err
		}
		e.log.Info("repository deleted on its primary; archiving the other copies", "repository", rec.FullName, "primary", primary.Name)
		e.audit(ctx, "repo.deleted_on_primary", rec.FullName, map[string]any{"repository_id": rec.ID, "primary": primary.Name})
	}
	archives, err := e.store.Archives(ctx, rec.ID)
	if err != nil {
		return false, err
	}
	owner, name, _ := strings.Cut(rec.FullName, "/")
	complete := true
	for _, node := range replicas {
		if !healthy[node] {
			record(node, StateWaiting, node+" isn't healthy; its copy is archived once it is", 0, nil)
			complete = false
			continue
		}
		n := e.nodes[node]
		if n.API == nil {
			record(node, StateMissing, "deleted on the primary; ForgeSync can't reach "+node+"'s API to archive its copy", 0, nil)
			complete = false
			continue
		}
		var a *store.Archive
		for i := range archives {
			x := &archives[i]
			if x.Node == node && x.State != "purged" && strings.EqualFold(x.OriginalName, rec.FullName) {
				a = x
			}
		}
		detail, err := e.archiveOn(ctx, rec, n, owner, name, a, now)
		if err != nil {
			record(node, StateError, "archiving the copy on "+node+" failed: "+err.Error(), 0, nil)
			complete = false
			continue
		}
		record(node, StateArchived, detail, 0, nil)
	}
	// Nothing is left to compare, so git conflicts end once every replica
	// was dealt with.
	checked := []string{}
	if complete {
		checked = []string{rec.ID}
	}
	return complete, e.syncConflicts(ctx, rec, nil, checked)
}

// archiveOn archives one replica's copy, resuming where an earlier attempt
// stopped. It returns the replica's state detail.
func (e *Engine) archiveOn(ctx context.Context, rec store.RepositoryRecord, n Node, owner, name string, a *store.Archive,
	now time.Time) (string, error) {

	org := e.opts.ArchiveOrg
	if a == nil {
		_, found, err := n.API.GetRepo(ctx, owner, name)
		if err != nil {
			return "", err
		}
		if !found {
			return "deleted on the primary; there's no copy on " + n.Name, nil
		}
		// Renaming first keeps names unique in the archive organization.
		archived := archiveName(owner, name, now)
		if err := n.API.EditRepo(ctx, owner, name, forgejo.EditRepoOption{Name: &archived}); err != nil {
			return "", fmt.Errorf("renaming it: %w", err)
		}
		a = &store.Archive{RepositoryID: rec.ID, Node: n.Name, OriginalName: rec.FullName, ArchivedName: archived,
			State: "renamed", ArchivedAt: now}
		id, err := e.store.SaveArchive(ctx, *a)
		if err != nil {
			return "", err
		}
		a.ID = id
	}
	if a.State == "renamed" {
		if err := e.ensureArchiveOrg(ctx, n); err != nil {
			return "", err
		}
		if _, there, err := n.API.GetRepo(ctx, org, a.ArchivedName); err != nil {
			return "", err
		} else if !there {
			if err := n.API.TransferRepo(ctx, owner, a.ArchivedName, org); err != nil {
				return "", fmt.Errorf("moving it to %s: %w", org, err)
			}
			if _, there, err = n.API.GetRepo(ctx, org, a.ArchivedName); err != nil {
				return "", err
			} else if !there {
				return "", fmt.Errorf("moving it to %s is waiting for acceptance; the service account must own %s", org, org)
			}
		}
		yes := true
		if err := n.API.EditRepo(ctx, org, a.ArchivedName, forgejo.EditRepoOption{Archived: &yes}); err != nil {
			return "", fmt.Errorf("marking it archived: %w", err)
		}
		until := now.Add(e.opts.BackupFor)
		a.State, a.DeleteAfter = "archived", &until
		if _, err := e.store.SaveArchive(ctx, *a); err != nil {
			return "", err
		}
		e.log.Info("copy archived", "repository", rec.FullName, "node", n.Name, "as", org+"/"+a.ArchivedName)
		e.audit(ctx, "repo.archived_on_node", rec.FullName, map[string]any{
			"repository_id": rec.ID, "node": n.Name, "archived_as": org + "/" + a.ArchivedName, "delete_after": until})
	}
	return fmt.Sprintf("deleted on the primary; archived as %s/%s until %s", org, a.ArchivedName, a.DeleteAfter.Format("2006-01-02")), nil
}

func (e *Engine) ensureArchiveOrg(ctx context.Context, n Node) error {
	exists, err := n.API.IsOrg(ctx, e.opts.ArchiveOrg)
	if err != nil || exists {
		return err
	}
	err = n.API.AdminCreateOrg(ctx, n.User, forgejo.CreateOrgOption{
		UserName: e.opts.ArchiveOrg, FullName: "ForgeSync archive", Visibility: "private",
		Description: "Copies of repositories deleted on their primary, kept for a while before ForgeSync deletes them.",
	})
	if err != nil && !forgejo.IsConflict(err) {
		return fmt.Errorf("creating the organization %s: %w", e.opts.ArchiveOrg, err)
	}
	return nil
}

// archiveName is <owner>--<name>--<time>, within Forgejo's 100 characters.
func archiveName(owner, name string, at time.Time) string {
	suffix := "--" + at.Format("20060102-150405")
	base := owner + "--" + name
	if len(base)+len(suffix) > 100 {
		base = base[:100-len(suffix)]
	}
	return base + suffix
}

// purgeArchives deletes archived copies whose backup period is over. It
// reports whether the repository was deleted on its primary and nothing of
// it is left anywhere, so ForgeSync can forget it.
func (e *Engine) purgeArchives(ctx context.Context, rec store.RepositoryRecord, healthy map[string]bool) (bool, error) {
	archives, err := e.store.Archives(ctx, rec.ID)
	if err != nil {
		return false, err
	}
	now := e.now().UTC()
	if len(archives) == 0 && (rec.DeletedAt == nil || now.Before(rec.DeletedAt.Add(e.opts.BackupFor))) {
		// Nothing to delete. A deleted repository no node had a copy of is
		// still listed for the backup period.
		return false, nil
	}
	left := 0
	for _, a := range archives {
		if a.State == "purged" {
			continue
		}
		n, ok := e.nodes[a.Node]
		if a.State != "archived" || a.DeleteAfter == nil || now.Before(*a.DeleteAfter) || !ok || n.API == nil || !healthy[a.Node] {
			left++
			continue
		}
		// An archive somebody has already deleted by hand is done with,
		// not a failure. Counting it as one left the repository waiting to
		// be forgotten for ever, and its git cache with it.
		if err := n.API.DeleteRepo(ctx, e.opts.ArchiveOrg, a.ArchivedName); err != nil && !forgejo.IsNotFound(err) {
			e.log.Warn("deleting an archived copy failed", "repository", rec.FullName, "node", a.Node, "error", err)
			left++
			continue
		}
		a.State = "purged"
		if _, err := e.store.SaveArchive(ctx, a); err != nil {
			return false, err
		}
		e.log.Info("archived copy deleted", "repository", rec.FullName, "node", a.Node, "archive", a.ArchivedName)
		e.audit(ctx, "repo.archive_deleted", rec.FullName, map[string]any{
			"repository_id": rec.ID, "node": a.Node, "archived_as": e.opts.ArchiveOrg + "/" + a.ArchivedName})
	}
	if left > 0 || rec.DeletedAt == nil {
		return false, nil
	}
	for _, r := range rec.Replicas {
		if r.Present { // a copy the inventory still sees somewhere
			return false, nil
		}
	}
	return true, nil
}
