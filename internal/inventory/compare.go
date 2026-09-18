// Package inventory discovers which repositories exist on which Forgejo node
// and compares them. It only observes; it doesn't replicate anything.
package inventory

import (
	"scenegit.org/forgesync/internal/store"
)

// Status compares a repository across all configured nodes.
type Status string

const (
	// Same: on every node, same default branch at the same commit.
	Same Status = "same"
	// Differs: on every node, but the default branch or its commit differs.
	Differs Status = "differs"
	// Missing: at least one node that was scanned doesn't have it.
	Missing Status = "missing"
	// Unknown: some node hasn't been scanned since the repository appeared,
	// or a branch head couldn't be read.
	Unknown Status = "unknown"
)

// Presence is what one node has of a repository.
type Presence string

const (
	Present     Presence = "present"
	Absent      Presence = "absent"
	NotKnownYet Presence = "unknown"
)

// NodeView is one node's side of a repository.
type NodeView struct {
	Node     string         `json:"node"`
	Presence Presence       `json:"presence"`
	Stale    bool           `json:"stale"` // the node's latest scan failed; this is older data
	Replica  *store.Replica `json:"replica,omitempty"`
}

// Compare works out a repository's status across nodes, given each node's
// latest scan.
func Compare(rec store.RepositoryRecord, nodes []string, scans map[string]store.NodeScan) (Status, []NodeView) {
	replicas := map[string]store.Replica{}
	for _, r := range rec.Replicas {
		replicas[r.Node] = r
	}
	views := make([]NodeView, 0, len(nodes))
	var missing, unknown bool
	var present []store.Replica
	for _, n := range nodes {
		v := NodeView{Node: n, Presence: NotKnownYet}
		scan, scanned := scans[n]
		v.Stale = scanned && !scan.OK
		r, seen := replicas[n]
		switch {
		case seen && r.Present:
			v.Presence = Present
			rc := r
			v.Replica = &rc
			present = append(present, r)
		case seen:
			v.Presence = Absent
			rc := r
			v.Replica = &rc
			missing = true
		case scanned && scan.LastSuccessAt != nil && !scan.LastSuccessAt.Before(rec.FirstSeenAt):
			// Scanned successfully since the repository appeared, and not found.
			v.Presence = Absent
			missing = true
		default:
			unknown = true
		}
		views = append(views, v)
	}

	switch {
	case missing:
		return Missing, views
	case unknown || len(present) == 0:
		return Unknown, views
	}
	first := present[0]
	for _, r := range present {
		if r.HeadError != "" || (!r.Empty && r.HeadSHA == "") {
			return Unknown, views
		}
	}
	for _, r := range present[1:] {
		if r.Empty != first.Empty || r.DefaultBranch != first.DefaultBranch || r.HeadSHA != first.HeadSHA {
			return Differs, views
		}
	}
	return Same, views
}
