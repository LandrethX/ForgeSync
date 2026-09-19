package issues

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestPlanSet(t *testing.T) {
	set := func(elements ...string) map[string]bool {
		out := map[string]bool{}
		for _, e := range elements {
			out[e] = true
		}
		return out
	}
	show := func(m map[string][]string) string {
		nodes := slices.Sorted(maps.Keys(m))
		var out []string
		for _, n := range nodes {
			out = append(out, n+"="+strings.Join(m[n], "+"))
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct {
		name        string
		have        map[string]map[string]bool
		base        map[string]bool
		add, remove string
	}{
		{
			name: "everyone agrees: nothing to do",
			have: map[string]map[string]bool{"se": set("alice:+1"), "dk": set("alice:+1")},
			base: set("alice:+1"),
		},
		{
			name: "added on one node: added on the others",
			have: map[string]map[string]bool{"se": set("alice:+1"), "dk": set(), "de": set()},
			base: set(),
			add:  "de=alice:+1 dk=alice:+1",
		},
		{
			name:   "taken back on one node: taken back on the others",
			have:   map[string]map[string]bool{"se": set(), "dk": set("alice:+1"), "de": set("alice:+1")},
			base:   set("alice:+1"),
			remove: "de=alice:+1 dk=alice:+1",
		},
		{
			name: "one added and another taken back at once: both, and no conflict",
			have: map[string]map[string]bool{
				"se": set("bob:heart"), "dk": set("alice:+1", "bob:heart"), "de": set("alice:+1")},
			base:   set("alice:+1"),
			add:    "de=bob:heart",
			remove: "de=alice:+1 dk=alice:+1",
		},
		{
			name: "two people react at once: both spread",
			have: map[string]map[string]bool{"se": set("alice:+1"), "dk": set("bob:rocket")},
			base: set(),
			add:  "dk=alice:+1 se=bob:rocket",
		},
		{
			name: "the same reaction on a node that has no copy yet",
			have: map[string]map[string]bool{"se": set("alice:+1"), "dk": nil},
			base: set(),
			add:  "dk=alice:+1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planSet(tc.have, tc.base)
			if got := show(p.add); got != tc.add {
				t.Errorf("add = %q, want %q", got, tc.add)
			}
			if got := show(p.remove); got != tc.remove {
				t.Errorf("remove = %q, want %q", got, tc.remove)
			}
		})
	}
}

func TestSettleSetOnlyMovesTheBaseWhenEveryNodeAgrees(t *testing.T) {
	set := func(elements ...string) map[string]bool {
		out := map[string]bool{}
		for _, e := range elements {
			out[e] = true
		}
		return out
	}
	all := map[string]map[string]bool{"se": set("alice:+1"), "dk": set("alice:+1")}
	if got := settleSet(all, set()); got != "alice:+1" {
		t.Errorf("everyone has it: %q", got)
	}
	none := map[string]map[string]bool{"se": set(), "dk": set()}
	if got := settleSet(none, set("alice:+1")); got != "" {
		t.Errorf("nobody has it: %q", got)
	}
	// Half-written: the base doesn't move, so the next run tries again
	// instead of reading the node that's behind as someone's change.
	half := map[string]map[string]bool{"se": set("alice:+1"), "dk": set()}
	if got := settleSet(half, set()); got != "" {
		t.Errorf("half added: %q", got)
	}
	if got := settleSet(half, set("alice:+1")); got != "alice:+1" {
		t.Errorf("half removed: %q", got)
	}
}

func TestReactionsSpreadBothWays(t *testing.T) {
	s, st, f, _ := setup(t)
	s.opts.Reactions = true
	f["se"].open("alice", "worth a reaction")
	s.run(t)
	f["se"].react(1, "alice", "+1")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reactionsOn(1); got != "alice:+1" {
			t.Fatalf("%s: %q", n, got)
		}
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}

	// One added on a replica and another taken back elsewhere at once:
	// both carry, and neither is a conflict.
	f["dk"].react(1, "bob", "heart")
	f["de"].unreact(1, "alice", "+1")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reactionsOn(1); got != "bob:heart" {
			t.Errorf("%s: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

func TestCommentReactionsSpread(t *testing.T) {
	s, _, f, _ := setup(t)
	s.opts.Reactions = true
	f["se"].open("alice", "with a comment")
	id := f["se"].say("bob", 1, "good point").ID
	s.run(t)
	f["se"].mu.Lock()
	f["se"].set(true, id)["carol:rocket"] = true
	f["se"].mu.Unlock()
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		ids := f[n].commentIDs(1)
		if len(ids) != 1 {
			t.Fatalf("%s has %d comments", n, len(ids))
		}
		if got := f[n].commentReactionsOn(ids[0]); got != "carol:rocket" {
			t.Errorf("%s comment: %q", n, got)
		}
	}
}

// A person ForgeSync can't create on a node, or an emoji that node won't
// take, leaves the reaction half-copied. That must never be read as someone
// taking it back everywhere; it's tried again until it lands.
func TestAReactionANodeWontTake(t *testing.T) {
	s, st, f, _ := setup(t)
	s.opts.Reactions = true
	f["de"].refuses = "carol"
	f["se"].open("alice", "for carol")
	s.run(t)
	f["se"].react(1, "carol", "eyes")
	for i := 0; i < 3; i++ {
		s.run(t)
		if got, other := f["se"].reactionsOn(1), f["dk"].reactionsOn(1); got != "carol:eyes" || other != "carol:eyes" {
			t.Fatalf("round %d: se %q dk %q", i, got, other)
		}
		if got := f["de"].reactionsOn(1); got != "" {
			t.Fatalf("round %d: de took it after all: %q", i, got)
		}
	}
	// Once the node will take it, it lands there too.
	f["de"].refuses = ""
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reactionsOn(1); got != "carol:eyes" {
			t.Errorf("%s: %q", n, got)
		}
	}

	// The same the other way: taken back where it can, kept where it can't,
	// and never resurrected on the nodes that dropped it.
	f["de"].refuses = "carol"
	f["se"].unreact(1, "carol", "eyes")
	for i := 0; i < 3; i++ {
		s.run(t)
		if got, other := f["se"].reactionsOn(1), f["dk"].reactionsOn(1); got != "" || other != "" {
			t.Fatalf("round %d: it came back: se %q dk %q", i, got, other)
		}
	}
	f["de"].refuses = ""
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reactionsOn(1); got != "" {
			t.Errorf("%s: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}
