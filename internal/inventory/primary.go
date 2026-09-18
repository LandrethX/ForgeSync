package inventory

import (
	"context"
	"log/slog"

	"scenegit.org/forgesync/internal/store"
)

// PrimaryStore is what AssignPrimaries needs from the store.
type PrimaryStore interface {
	AssignPrimaries(ctx context.Context, nodes []string) (store.Assignments, bool, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// AssignPrimaries applies the primary-site rules (see store.AssignPrimaries)
// and audits every change. It runs after every scan round, before conflict
// detection and replication look at the repositories.
func AssignPrimaries(ctx context.Context, db PrimaryStore, nodes []string, log *slog.Logger) error {
	res, ok, err := db.AssignPrimaries(ctx, nodes)
	if err != nil {
		return err
	}
	if !ok {
		log.Info("primaries not assigned this round: not every node's latest scan succeeded")
		return nil
	}
	audit := func(action, target string, details map[string]any) {
		if err := db.Audit(ctx, "forgesync", action, target, details); err != nil {
			log.Error("audit failed", "action", action, "error", err)
		}
	}
	for _, h := range res.Homes {
		log.Info("user's primary site set to registration site", "user", h.Login, "node", h.Node)
		audit("user.home_assigned", h.Login, map[string]any{"user_id": h.UserID, "to": h.Node, "reason": "registration"})
	}
	for _, p := range res.Primaries {
		log.Info("repository primary set", "repository", p.FullName, "from", p.From, "to", p.To, "reason", p.Reason)
		audit("repo.primary_assigned", p.FullName, map[string]any{
			"repository_id": p.RepositoryID, "from": p.From, "to": p.To, "reason": p.Reason})
	}
	return nil
}
