// Package issues replicates issues and their comments between the nodes.
//
// Every issue and comment gets one identity (store.IssueRecord and
// CommentRecord) with a copy per node. Forgejo assigns numbers and ids
// itself (Phase 0 p04), so copies are created in creation order, which keeps
// numbers aligned whenever that's possible, and the mapping records them
// when it isn't (e.g. a pull request took the number on one node).
//
// Issues and comments can be created and edited on any node. Title, body
// and state are merged per field against the last value that agreed
// everywhere (the base): one new value anywhere is copied everywhere; two
// different new values are a conflict and nothing is overwritten. A copy
// deleted on the primary is deleted elsewhere if unchanged since; a copy
// deleted elsewhere is recreated from the primary, as for repositories.
//
// ForgeSync creates copies as their author (Sudo), and edits as its service
// account. Its own writes come back as webhooks that look like the author's
// (p05), so loops end by comparison: a run that finds everything agreeing
// writes nothing.
package issues

import "sort"

// merge decides one field across the nodes that have a copy. vals maps a
// node to its current value. It returns the value everyone should have and
// the nodes to update, or conflict with the distinct new values.
func merge(vals map[string]string, base string) (value string, writes []string, conflict bool) {
	changed := map[string]bool{}
	for _, v := range vals {
		if v != base {
			changed[v] = true
		}
	}
	switch len(changed) {
	case 0:
		return base, nil, false
	case 1:
		for v := range changed {
			value = v
		}
		for node, v := range vals {
			if v != value {
				writes = append(writes, node)
			}
		}
		sort.Strings(writes)
		return value, writes, false
	}
	return "", nil, true
}
