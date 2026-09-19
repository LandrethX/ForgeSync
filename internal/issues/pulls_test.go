package issues

import "testing"

// A pull request is replicated as a kind of issue -- its conversation,
// title and body -- but never its state.
func TestPullRequestsAndTheirComments(t *testing.T) {
	s, st, f, _ := setup(t)
	s.opts.PullRequests = true
	// The branches replication put on the other nodes.
	for _, n := range []string{"dk", "de"} {
		f[n].branches["topic/change"] = true
	}
	f["se"].openPull("alice", "Add notes", "topic/change", "main")
	f["se"].say("alice", 1, "Ready for review.")
	s.run(t)

	for _, n := range []string{"dk", "de"} {
		if got := f[n].pullOn(1); got != "open" {
			t.Fatalf("%s: pull request state %q", n, got)
		}
		if got := f[n].commentsOn(1); len(got) != 1 || got[0] != "alice: Ready for review." {
			t.Errorf("%s: comments %v", n, got)
		}
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}

	// The conversation goes both ways, like an issue's.
	f["de"].say("carol", 1, "One question.")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].commentsOn(1); len(got) != 2 {
			t.Errorf("%s: comments %v", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

// Merging is the owner's, on the node they chose. ForgeSync closes the
// copies to follow the primary and never merges or reopens one.
func TestAMergedPullRequestOnlyClosesTheCopies(t *testing.T) {
	s, _, f, _ := setup(t)
	s.opts.PullRequests = true
	for _, n := range []string{"dk", "de"} {
		f[n].branches["topic/change"] = true
	}
	f["se"].openPull("alice", "Add notes", "topic/change", "main")
	s.run(t)

	f["se"].mergePull(1)
	s.run(t)
	for _, n := range []string{"dk", "de"} {
		if got := f[n].pullOn(1); got != "closed" {
			t.Errorf("%s: %q, want closed", n, got)
		}
		// Closed, not merged: the merge commit is the primary's alone.
		f[n].mu.Lock()
		merged := f[n].pulls[1].Merged
		f[n].mu.Unlock()
		if merged {
			t.Errorf("%s: ForgeSync merged the copy", n)
		}
	}

	// Reopened on a replica: ForgeSync leaves the primary alone and doesn't
	// fight over it either.
	f["de"].mu.Lock()
	f["de"].pulls[1].State, f["de"].issues[1].State = "open", "open"
	f["de"].mu.Unlock()
	s.run(t)
	if got := f["se"].pullOn(1); got != "closed" {
		t.Errorf("the primary was reopened: %q", got)
	}
}

// Until the branches are there, the copy waits: replication puts them
// there, and a pull request without them can't be opened at all.
func TestAPullRequestWaitsForItsBranches(t *testing.T) {
	s, _, f, _ := setup(t)
	s.opts.PullRequests = true
	f["dk"].branches["topic/change"] = true // de hasn't got it yet
	f["se"].openPull("alice", "Add notes", "topic/change", "main")
	s.run(t)
	if got := f["dk"].pullOn(1); got != "open" {
		t.Errorf("dk: %q", got)
	}
	if got := f["de"].pullOn(1); got != "" {
		t.Errorf("de opened one without the branch: %q", got)
	}

	f["de"].branches["topic/change"] = true
	s.run(t)
	if got := f["de"].pullOn(1); got != "open" {
		t.Errorf("de after the branch arrived: %q", got)
	}
}

// With pull requests off, they and their comments are left alone, as
// before.
func TestPullRequestsAreLeftAloneWhenOff(t *testing.T) {
	s, st, f, _ := setup(t)
	f["se"].openPull("alice", "Add notes", "topic/change", "main")
	f["se"].say("alice", 1, "Ready for review.")
	f["se"].open("bob", "an ordinary issue")
	s.run(t)
	for _, n := range []string{"dk", "de"} {
		if got := f[n].pullOn(1); got != "" {
			t.Errorf("%s got a pull request: %q", n, got)
		}
		if f[n].byTitle("an ordinary issue") == nil {
			t.Errorf("%s: the issue didn't replicate", n)
		}
	}
	if len(st.issues) != 1 {
		t.Errorf("records = %d, want only the issue", len(st.issues))
	}
}
