package issues

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
)

func TestLabelsAndMilestones(t *testing.T) {
	s, st, f, _ := setup(t)
	f["dk"].label("bug", "ee0701")
	f["se"].label("feature", "#0e8a16") // with a # on one node: the same colour
	f["se"].milestone("v1")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if f[n].labelNamed("bug") == nil || f[n].labelNamed("feature") == nil || f[n].milestoneNamed("v1") == nil {
			t.Fatalf("%s: labels %v milestones %v", n, f[n].labels, f[n].milestones)
		}
	}
	if len(st.items) != 3 {
		t.Errorf("items = %d", len(st.items))
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a second run wrote %d times", n)
	}

	// Renamed on a replica: followed by id, not taken for a new label.
	f["de"].labelNamed("bug").Name = "defect"
	f["se"].labelNamed("feature").Color = "c2e0c6"
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if f[n].labelNamed("defect") == nil || f[n].labelNamed("bug") != nil || f[n].labelNamed("feature").Color != "c2e0c6" {
			t.Errorf("%s after edits: %v", n, f[n].labels)
		}
	}
	// Closed and given a due date on a replica: everywhere.
	due := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	f["dk"].milestoneNamed("v1").State = "closed"
	f["dk"].milestoneNamed("v1").Deadline = &due
	s.run(t)
	if m := f["de"].milestoneNamed("v1"); m.State != "closed" || m.Deadline == nil || !m.Deadline.Equal(due) {
		t.Errorf("de v1 = %+v", m)
	}

	// Deleted on a replica: recreated. Deleted on the primary: gone everywhere.
	feature := mustID(t, f["de"], "feature")
	f["de"].mu.Lock()
	delete(f["de"].labels, feature)
	f["de"].mu.Unlock()
	s.run(t)
	if f["de"].labelNamed("feature") == nil {
		t.Error("feature not recreated on de")
	}
	fakeAPI{f["se"], "alice"}.DeleteLabel(t.Context(), "", "", f["se"].labelNamed("defect").ID)
	s.run(t)
	for _, n := range []string{"dk", "de"} {
		if f[n].labelNamed("defect") != nil {
			t.Errorf("defect still on %s", n)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

func mustID(t *testing.T, f *fakeNode, name string) int64 {
	t.Helper()
	l := f.labelNamed(name)
	if l == nil {
		t.Fatalf("no label %s on %s", name, f.name)
	}
	return l.ID
}

func TestSameNamedLabelsAreOne(t *testing.T) {
	s, st, f, _ := setup(t, "se", "dk")
	f["se"].label("bug", "ee0701")
	f["dk"].label("bug", "ee0701")
	s.run(t)
	if len(st.items) != 1 || len(f["se"].labels) != 1 || len(f["dk"].labels) != 1 {
		t.Errorf("items %d, se %d, dk %d", len(st.items), len(f["se"].labels), len(f["dk"].labels))
	}
}

func TestIssueLabelsAndMilestone(t *testing.T) {
	s, st, f, _ := setup(t)
	bug := f["se"].label("bug", "ee0701")
	f["se"].label("docs", "0075ca")
	v1 := f["se"].milestone("v1")
	is := f["se"].open("alice", "with labels")
	f["se"].setLabels(is, []int64{bug})
	f["se"].setMilestone(is, v1)
	s.run(t)
	// New copies get the labels and milestone, with each node's own ids.
	for _, n := range []string{"dk", "de"} {
		if got := f[n].labelNames(1); got != "bug" || f[n].milestoneTitle(1) != "v1" {
			t.Fatalf("%s #1: labels %q milestone %q", n, got, f[n].milestoneTitle(1))
		}
	}

	// Labels changed on a replica, milestone removed on the primary: both
	// carry over.
	fakeAPI{f["dk"], "bob"}.ReplaceIssueLabels(t.Context(), "", "", 1, []int64{mustID(t, f["dk"], "bug"), mustID(t, f["dk"], "docs")})
	f["se"].setMilestone(f["se"].issue(1), 0)
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].labelNames(1); got != "bug,docs" || f[n].milestoneTitle(1) != "" {
			t.Errorf("%s #1: labels %q milestone %q", n, got, f[n].milestoneTitle(1))
		}
	}

	// Different label changes on two nodes: a conflict that names them.
	fakeAPI{f["se"], "alice"}.ReplaceIssueLabels(t.Context(), "", "", 1, []int64{bug})
	fakeAPI{f["de"], "carol"}.ReplaceIssueLabels(t.Context(), "", "", 1, []int64{mustID(t, f["de"], "docs")})
	s.run(t)
	if len(st.found) != 1 || st.found[0].Ref != "#1 labels" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if vals := st.found[0].Details["values"].(map[string]string); vals["se"] != "bug" || vals["de"] != "docs" || vals["dk"] != "bug, docs" {
		t.Errorf("conflict values = %v", vals)
	}
}

func TestOrganizationLabelsAreLeftAlone(t *testing.T) {
	s, _, f, _ := setup(t, "se", "dk")
	bug := f["se"].label("bug", "ee0701")
	f["se"].open("alice", "first")
	s.run(t)
	// On dk the issue also has an organization label (not in the
	// repository's own labels) when bug is added on se.
	f["dk"].mu.Lock()
	f["dk"].issues[1].Labels = []forgejo.Label{{ID: 777, Name: "org-wide"}}
	f["dk"].mu.Unlock()
	f["se"].setLabels(f["se"].issue(1), []int64{bug})
	s.run(t)
	bugOnDK := mustID(t, f["dk"], "bug")
	f["dk"].mu.Lock()
	var ids []int64
	for _, l := range f["dk"].issues[1].Labels {
		ids = append(ids, l.ID)
	}
	f["dk"].mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	want := []int64{bugOnDK, 777}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("dk #1 labels = %v, want %v (the organization's 777 kept)", ids, want)
	}
	if got := f["dk"].labelNames(1); got != "bug" {
		t.Errorf("dk #1 repository labels = %q", got)
	}
}

// Deleting a label on a replica takes it off that node's issues too.
// That's Forgejo tidying up after the deletion, not someone unlabelling the
// issue, so the label comes back and the issues keep it everywhere.
func TestLabelDeletedOnReplicaKeepsIssueLabels(t *testing.T) {
	s, st, f, _ := setup(t)
	bug := f["se"].label("bug", "ee0701")
	f["se"].setLabels(f["se"].open("alice", "labelled"), []int64{bug})
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].labelNames(1); got != "bug" {
			t.Fatalf("%s #1 before: %q", n, got)
		}
	}

	fakeAPI{f["de"], "carol"}.DeleteLabel(t.Context(), "", "", mustID(t, f["de"], "bug"))
	s.run(t)
	if f["de"].labelNamed("bug") == nil {
		t.Fatal("the label wasn't recreated on de")
	}
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].labelNames(1); got != "bug" {
			t.Errorf("%s #1 after the deletion on de: %q", n, got)
		}
	}
	// A second run finds everything agreeing.
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

// A label or milestone changed on a replica and then deleted on the primary
// stays there for its owner to settle -- and so does its place on that
// node's issues. Other labels still reach the node meanwhile.
func TestItemKeptForItsOwnerStaysOnTheIssues(t *testing.T) {
	s, st, f, _ := setup(t)
	bug := f["se"].label("bug", "ee0701")
	v1 := f["se"].milestone("v1")
	is := f["se"].open("alice", "labelled")
	f["se"].setLabels(is, []int64{bug})
	f["se"].setMilestone(is, v1)
	s.run(t)
	if f["de"].labelNames(1) != "bug" || f["de"].milestoneTitle(1) != "v1" {
		t.Fatalf("de #1 before: %q %q", f["de"].labelNames(1), f["de"].milestoneTitle(1))
	}

	// Changed on de and deleted on the primary in the same window.
	f["de"].labelNamed("bug").Color = "b60205"
	f["de"].milestoneNamed("v1").Description = "ships in May"
	fakeAPI{f["se"], "alice"}.DeleteLabel(t.Context(), "", "", f["se"].labelNamed("bug").ID)
	fakeAPI{f["se"], "alice"}.DeleteMilestone(t.Context(), "", "", f["se"].milestoneNamed("v1").ID)
	s.run(t)

	if f["de"].labelNamed("bug") == nil || f["de"].milestoneNamed("v1") == nil {
		t.Fatal("de's changed copies should be kept")
	}
	if got, ms := f["de"].labelNames(1), f["de"].milestoneTitle(1); got != "bug" || ms != "v1" {
		t.Errorf("de #1 kept: labels %q milestone %q", got, ms)
	}
	for _, n := range []string{"se", "dk"} {
		if f[n].labelNamed("bug") != nil || f[n].milestoneNamed("v1") != nil {
			t.Errorf("%s still has them", n)
		}
		if got, ms := f[n].labelNames(1), f[n].milestoneTitle(1); got != "" || ms != "" {
			t.Errorf("%s #1: labels %q milestone %q", n, got, ms)
		}
	}
	var refs []string
	for _, c := range st.found {
		refs = append(refs, c.Ref)
	}
	sort.Strings(refs)
	if len(refs) != 2 || refs[0] != "label bug deleted" || refs[1] != "milestone v1 deleted" {
		t.Errorf("conflicts = %v", refs)
	}

	// A settled run leaves de's issue as it is.
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}
	if got, ms := f["de"].labelNames(1), f["de"].milestoneTitle(1); got != "bug" || ms != "v1" {
		t.Errorf("de #1 after a settled run: labels %q milestone %q", got, ms)
	}

	// A new label still reaches de, next to the one it's keeping.
	docs := f["se"].label("docs", "0075ca")
	f["se"].setLabels(f["se"].issue(1), []int64{docs})
	s.run(t)
	if got := f["de"].labelNames(1); got != "bug,docs" {
		t.Errorf("de #1 with a new label: %q", got)
	}
	for _, n := range []string{"se", "dk"} {
		if got := f[n].labelNames(1); got != "docs" {
			t.Errorf("%s #1 with a new label: %q", n, got)
		}
	}

	// Deleted on de as well: both are forgotten and the conflicts clear.
	fakeAPI{f["de"], "carol"}.DeleteLabel(t.Context(), "", "", f["de"].labelNamed("bug").ID)
	fakeAPI{f["de"], "carol"}.DeleteMilestone(t.Context(), "", "", f["de"].milestoneNamed("v1").ID)
	s.run(t)
	if len(st.found) != 0 {
		t.Errorf("conflicts left: %+v", st.found)
	}
	if len(st.items) != 1 { // docs
		t.Errorf("items left: %+v", st.items)
	}
}

func TestIssueAssignees(t *testing.T) {
	s, st, f, _ := setup(t)
	f["se"].open("alice", "needs someone")
	f["se"].setAssignees(1, "alice")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].assignees(1); got != "alice" {
			t.Fatalf("%s #1: %q", n, got)
		}
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}

	// Another one added on a replica: everywhere.
	f["dk"].setAssignees(1, "alice", "bob")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].assignees(1); got != "alice,bob" {
			t.Errorf("%s #1 after dk: %q", n, got)
		}
	}
	// Cleared on the primary: everywhere.
	f["se"].setAssignees(1)
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].assignees(1); got != "" {
			t.Errorf("%s #1 after clearing: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Fatalf("conflicts: %+v", st.found)
	}

	// Two nodes assign differently: a conflict, and nothing is written.
	f["se"].setAssignees(1, "alice")
	f["de"].setAssignees(1, "carol")
	s.run(t)
	if len(st.found) != 1 || st.found[0].Ref != "#1 assignees" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if vals := st.found[0].Details["values"].(map[string]string); vals["se"] != "alice" || vals["de"] != "carol" || vals["dk"] != "" {
		t.Errorf("values = %v", vals)
	}
	if got := f["dk"].assignees(1); got != "" {
		t.Errorf("dk was written during a conflict: %q", got)
	}
}

// Forgejo only assigns people with write access, and ForgeSync doesn't
// replicate collaborators yet, so a node can be unable to hold an assignee.
// It stays a conflict for a person, and nothing is written anywhere: the
// node that's behind must not win a later comparison.
func TestAnAssigneeANodeWontTake(t *testing.T) {
	s, st, f, _ := setup(t)
	delete(f["de"].assignable, "bob")
	f["se"].open("alice", "needs bob")
	f["se"].setAssignees(1, "bob")
	s.run(t)
	if len(st.found) != 1 || st.found[0].Ref != "#1 assignees" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if blocked := st.found[0].Details["blocked"].([]string); len(blocked) != 1 || blocked[0] != "de: bob can't be given an issue there" {
		t.Errorf("blocked = %v", blocked)
	}
	// A copy is made to match the primary where the node will take it; de's
	// is made without bob, and nothing takes him off the others.
	if got, other := f["se"].assignees(1), f["dk"].assignees(1); got != "bob" || other != "bob" {
		t.Errorf("se %q dk %q", got, other)
	}
	if got := f["de"].assignees(1); got != "" {
		t.Errorf("de #1: %q", got)
	}
	// However often it runs, de's emptiness never wins.
	for i := 0; i < 3; i++ {
		s.run(t)
	}
	if got, other := f["se"].assignees(1), f["dk"].assignees(1); got != "bob" || other != "bob" {
		t.Fatalf("bob was lost: se %q dk %q", got, other)
	}

	// Given access there, the next run settles it everywhere.
	f["de"].assignable["bob"] = true
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].assignees(1); got != "bob" {
			t.Errorf("%s #1: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts left: %+v", st.found)
	}
}

// A copy made on a node that won't take the assignees is made without them,
// and that doesn't take them off the others.
func TestACopyIsMadeWithoutAssigneesTheNodeWontTake(t *testing.T) {
	s, _, f, _ := setup(t)
	delete(f["de"].assignable, "carol")
	f["se"].open("alice", "for carol")
	f["se"].setAssignees(1, "carol")
	s.run(t)
	if got := f["dk"].assignees(1); got != "carol" {
		t.Errorf("dk #1: %q", got)
	}
	if got := f["de"].assignees(1); got != "" {
		t.Errorf("de #1 should have been made without them: %q", got)
	}
	s.run(t)
	if got := f["se"].assignees(1); got != "carol" {
		t.Errorf("se lost carol: %q", got)
	}
}
