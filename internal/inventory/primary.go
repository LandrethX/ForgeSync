package inventory

import (
	"context"
	"log/slog"
	"time"

	"scenegit.org/forgesync/internal/store"
)

// OriginStore is what AssignOrigins needs from the store.
type OriginStore interface {
	AssignOriginPrimaries(ctx context.Context, nodes []string) ([]store.OriginAssignment, bool, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// AssignOrigins gives every repository without a primary the node it was
// created on first, and audits each assignment. It runs after every scan
// round, so a new repository has a primary before conflict detection and
// replication look at it. An Administrator can change it afterwards.
func AssignOrigins(ctx context.Context, db OriginStore, nodes []string, log *slog.Logger) error {
	assigned, ok, err := db.AssignOriginPrimaries(ctx, nodes)
	if err != nil {
		return err
	}
	if !ok {
		log.Info("primaries not assigned this round: not every node's latest scan succeeded")
		return nil
	}
	for _, a := range assigned {
		created := map[string]string{}
		for n, t := range a.Nodes {
			created[n] = t.UTC().Format(time.RFC3339)
		}
		log.Info("primary set to origin", "repository", a.FullName, "node", a.Node)
		if err := db.Audit(ctx, "forgesync", "repo.primary_assigned", a.FullName, map[string]any{
			"repository_id": a.RepositoryID, "to": a.Node, "reason": "origin", "created_at": created,
		}); err != nil {
			log.Error("audit failed", "action", "repo.primary_assigned", "error", err)
		}
	}
	return nil
}
