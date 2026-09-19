package issues

import (
	"sort"
	"strings"
)

// Some of what hangs off an issue is a set rather than one value: its
// reactions, its attachments. Each member is either there or not, and the
// base says which way it moved, so they merge member by member: one that
// appears anywhere was added and is added everywhere, one missing anywhere
// was taken away and is taken away everywhere. Two people can't disagree
// about the same member, so unlike a title or a set of labels this can't
// become a conflict.
//
// A member enters the base only once every node has it, and leaves only
// once none has. So one ForgeSync couldn't write stays half-done and is
// tried again next run, instead of its absence being read as someone's
// change and copied everywhere.

// setValue is a set as the base records it: sorted members, comma-separated.
func setValue(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for e := range set {
		if set[e] {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func setFromValue(value string) map[string]bool {
	out := map[string]bool{}
	for _, e := range split(value) {
		out[e] = true
	}
	return out
}

// setPlan is what each node needs to catch up.
type setPlan struct {
	add    map[string][]string // node -> members to add there
	remove map[string][]string // node -> members to take away there
}

// planSet decides every member across the nodes that have a copy. have maps
// a node to what it has; base is what they last all agreed on.
func planSet(have map[string]map[string]bool, base map[string]bool) setPlan {
	p := setPlan{add: map[string][]string{}, remove: map[string][]string{}}
	for _, e := range members(have, base) {
		var with, without []string
		for n, set := range have {
			if set[e] {
				with = append(with, n)
			} else {
				without = append(without, n)
			}
		}
		switch {
		case base[e] && len(without) > 0:
			// It was there for everyone and is gone somewhere: taken away.
			for _, n := range with {
				p.remove[n] = append(p.remove[n], e)
			}
		case !base[e] && len(with) > 0:
			// It was nowhere and is somewhere: added.
			for _, n := range without {
				p.add[n] = append(p.add[n], e)
			}
		}
	}
	for n := range p.add {
		sort.Strings(p.add[n])
	}
	for n := range p.remove {
		sort.Strings(p.remove[n])
	}
	return p
}

// settleSet is the base after a round of writes: a member counts as agreed
// only once every node has it, and as gone only once none has. Anything
// half-done keeps its old base and is tried again next run.
func settleSet(have map[string]map[string]bool, base map[string]bool) string {
	out := map[string]bool{}
	for _, e := range members(have, base) {
		all, none := true, true
		for _, set := range have {
			if set[e] {
				none = false
			} else {
				all = false
			}
		}
		switch {
		case len(have) > 0 && all:
			out[e] = true
		case len(have) > 0 && none:
			// gone everywhere: out of the base
		case base[e]:
			out[e] = true // half-done: the base stays where it was
		}
	}
	return setValue(out)
}

// members is every member the base or any node knows, in a settled order.
func members(have map[string]map[string]bool, base map[string]bool) []string {
	seen := map[string]bool{}
	for e := range base {
		seen[e] = true
	}
	for _, set := range have {
		for e := range set {
			seen[e] = true
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
