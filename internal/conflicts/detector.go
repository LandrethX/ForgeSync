// Package conflicts finds conflicts between nodes that ForgeSync must not
// resolve on its own. For now that's the default branch of each repository:
//
//   - git_diverged: two nodes each have commits the other lacks, so neither
//     can be fast-forwarded to the other without losing work.
//   - default_branch_mismatch: nodes use different default branches.
//
// A node that is merely behind another is not a conflict: replication will
// fast-forward it. Metadata conflicts come with metadata replication.
package conflicts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/inventory"
	"scenegit.org/forgesync/internal/store"
)

const (
	KindDiverged       = "git_diverged"
	KindBranchMismatch = "default_branch_mismatch"
)

// Relation between two nodes' heads.
const (
	RelDiverged = "diverged"
	RelABehind  = "a_behind_b" // b contains a: a can be fast-forwarded
	RelBBehind  = "b_behind_a"
)

// Comparer is the part of the Forgejo client the detector needs.
type Comparer interface {
	CommitsAhead(ctx context.Context, owner, repo, base, head string) (int, bool, error)
}

// Store is what the detector reads and writes.
type Store interface {
	Repositories(ctx context.Context) ([]store.RepositoryRecord, error)
	NodeScans(ctx context.Context) ([]store.NodeScan, error)
	SyncConflicts(ctx context.Context, found []store.FoundConflict, checked, kinds []string, at time.Time) ([]store.ConflictChange, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// Detector compares what the nodes hold and records the differences a
// person has to settle. It never changes anything on a node.
type Detector struct {
	nodes   []string
	clients map[string]Comparer
	store   Store
	log     *slog.Logger
	now     func() time.Time

	// ReplicationOwnsPrimaries: when replication is on, it compares every
	// ref of repositories that have a primary, and owns their git_*
	// conflicts; the detector then only checks their default branch names.
	ReplicationOwnsPrimaries bool
}

// NewDetector checks the named nodes through their Forgejo clients and
// records what it finds in st.
func NewDetector(nodes []string, clients map[string]Comparer, st Store, log *slog.Logger) *Detector {
	return &Detector{nodes: nodes, clients: clients, store: st, log: log, now: time.Now}
}

// Run checks every repository and records the conflicts found.
func (d *Detector) Run(ctx context.Context) error {
	recs, err := d.store.Repositories(ctx)
	if err != nil {
		return err
	}
	scanList, err := d.store.NodeScans(ctx)
	if err != nil {
		return err
	}
	scans := map[string]store.NodeScan{}
	for _, s := range scanList {
		scans[s.Node] = s
	}

	// Two scopes: repositories the detector fully owns, and (with
	// replication on) repositories with a primary, where it only owns
	// default-branch mismatches.
	var found, foundPrimaried []store.FoundConflict
	var checked, checkedPrimaried []string
	for _, rec := range recs {
		f, conclusive := d.check(ctx, rec, scans)
		if d.ReplicationOwnsPrimaries && rec.PrimaryNode != "" {
			for _, c := range f {
				if c.Kind == KindBranchMismatch {
					foundPrimaried = append(foundPrimaried, c)
				}
			}
			if conclusive {
				checkedPrimaried = append(checkedPrimaried, rec.ID)
			}
			continue
		}
		found = append(found, f...)
		if conclusive {
			checked = append(checked, rec.ID)
		}
	}
	at := d.now().UTC()
	changes, err := d.store.SyncConflicts(ctx, found, checked, []string{KindDiverged, KindBranchMismatch}, at)
	if err != nil {
		return err
	}
	if d.ReplicationOwnsPrimaries {
		more, err := d.store.SyncConflicts(ctx, foundPrimaried, checkedPrimaried, []string{KindBranchMismatch}, at)
		if err != nil {
			return err
		}
		changes = append(changes, more...)
	}
	for _, c := range changes {
		action := "conflict." + c.Change // conflict.opened / conflict.cleared
		d.log.Info("conflict "+c.Change, "repository", c.FullName, "kind", c.Kind, "ref", c.Ref, "id", c.ID)
		if err := d.store.Audit(ctx, "forgesync", action, c.FullName,
			map[string]any{"conflict_id": c.ID, "kind": c.Kind, "ref": c.Ref}); err != nil {
			d.log.Error("writing audit log failed", "error", err)
		}
	}
	return nil
}

type side struct {
	node   string
	branch string
	head   string
}

// check returns the conflicts in one repository, and whether the check was
// conclusive (every node's data fresh and every comparison answered), which
// is what allows clearing conflicts that are gone.
func (d *Detector) check(ctx context.Context, rec store.RepositoryRecord, scans map[string]store.NodeScan) ([]store.FoundConflict, bool) {
	_, views := inventory.Compare(rec, d.nodes, scans)
	conclusive := true
	var sides []side
	for _, v := range views {
		if v.Presence != inventory.Present {
			continue // missing or not scanned: a replication matter, not a conflict
		}
		r := v.Replica
		if v.Stale || r.HeadError != "" {
			conclusive = false
			continue
		}
		if r.Empty {
			continue // an empty copy is behind everything
		}
		sides = append(sides, side{node: v.Node, branch: r.DefaultBranch, head: r.HeadSHA})
	}
	if len(sides) < 2 {
		return nil, conclusive
	}

	branches := map[string]string{}
	for _, s := range sides {
		branches[s.node] = s.branch
	}
	if !allSame(sides, func(s side) string { return s.branch }) {
		return []store.FoundConflict{{
			RepositoryID: rec.ID, Kind: KindBranchMismatch,
			Details: map[string]any{"branches": branches, "primary": rec.PrimaryNode},
		}}, conclusive
	}
	if allSame(sides, func(s side) string { return s.head }) {
		return nil, conclusive
	}

	owner, name, _ := strings.Cut(rec.FullName, "/")
	heads := map[string]string{}
	for _, s := range sides {
		heads[s.node] = s.head
	}
	var relations []map[string]string
	diverged := false
	for i := 0; i < len(sides); i++ {
		for j := i + 1; j < len(sides); j++ {
			a, b := sides[i], sides[j]
			if a.head == b.head {
				continue
			}
			rel, err := d.relation(ctx, owner, name, a, b)
			if err != nil {
				d.log.Warn("comparing heads failed", "repository", rec.FullName, "a", a.node, "b", b.node, "error", err)
				conclusive = false
				continue
			}
			relations = append(relations, map[string]string{"a": a.node, "b": b.node, "relation": rel})
			if rel == RelDiverged {
				diverged = true
			}
		}
	}
	if !diverged {
		return nil, conclusive
	}
	sort.Slice(relations, func(i, j int) bool { return relations[i]["a"]+relations[i]["b"] < relations[j]["a"]+relations[j]["b"] })
	return []store.FoundConflict{{
		RepositoryID: rec.ID, Kind: KindDiverged, Ref: "refs/heads/" + sides[0].branch,
		Details: map[string]any{"branch": sides[0].branch, "heads": heads, "relations": relations, "primary": rec.PrimaryNode},
	}}, conclusive
}

// relation compares two nodes' heads using each node's own history: node b
// can tell whether a's head is contained in b's head, and vice versa.
func (d *Detector) relation(ctx context.Context, owner, name string, a, b side) (string, error) {
	aExtra, err := d.hasCommitsNotIn(ctx, b.node, owner, name, b.head, a.head)
	if err != nil {
		return "", err
	}
	bExtra, err := d.hasCommitsNotIn(ctx, a.node, owner, name, a.head, b.head)
	if err != nil {
		return "", err
	}
	switch {
	case aExtra && bExtra:
		return RelDiverged, nil
	case aExtra:
		return RelBBehind, nil
	case bExtra:
		return RelABehind, nil
	}
	return "", errors.New("heads differ but neither has commits the other lacks")
}

// hasCommitsNotIn asks node whether head has commits that base doesn't. If
// node doesn't have head at all, it certainly does.
func (d *Detector) hasCommitsNotIn(ctx context.Context, node, owner, name, base, head string) (bool, error) {
	c, ok := d.clients[node]
	if !ok {
		return false, fmt.Errorf("no client for node %s", node)
	}
	n, found, err := c.CommitsAhead(ctx, owner, name, base, head)
	if err != nil {
		return false, fmt.Errorf("%s: %w", node, err)
	}
	return !found || n > 0, nil
}

func allSame(sides []side, key func(side) string) bool {
	for _, s := range sides[1:] {
		if key(s) != key(sides[0]) {
			return false
		}
	}
	return true
}
