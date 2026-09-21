package replication

import "scenegit.org/forgesync/internal/set"

// A set merge remembers what the nodes last agreed on, and that base is
// what says which way a member moved: one that appears where the base
// hasn't got it was added, one missing where the base has it was taken
// away. So the base is only true of the nodes that were actually read.
//
// A node that is unhealthy, or whose read failed, is not in the set at
// all. Settling the base without it means that when it comes back,
// everything it hasn't got reads as someone's deletion and is taken off
// every other node: on packages that deletes published files, on
// collaborators it takes away people's access, on branch protection it
// removes the rules. The node was never asked, and its silence is read
// as an answer.
//
// So the base waits. It costs nothing: the same comparison is made again
// next run against the same base, and writes nothing that was already
// written. The only thing that waits with it is a real deletion, which
// reaches the other nodes once the node that was away can be read.
//
// A node that answered "this feature is turned off here" is a different
// thing and is not counted: it has nothing to say and never will, which
// is an answer rather than a failure.

// settle returns the base to record: the newly settled one when every
// node that has the thing was read, and the one there was otherwise.
func settle(unread int, was string, have map[string]map[string]bool, base map[string]bool) string {
	if unread > 0 {
		return was
	}
	return set.Settle(have, base)
}
