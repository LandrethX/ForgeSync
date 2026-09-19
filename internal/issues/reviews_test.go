package issues

import (
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
)

func TestReviewMemberIsTheSameWhereverItWasWritten(t *testing.T) {
	// The same review as two nodes hold it: different ids, different line
	// comment ids, everything that matters identical.
	on := func(id int64) review {
		return review{
			head: forgejo.PullReview{ID: id, Reviewer: &forgejo.User{Login: "bob"}, State: "REQUEST_CHANGES",
				Body: "Not yet.", CommitID: "abc123"},
			comments: []forgejo.PullReviewComment{
				{ID: id * 10, Path: "a.go", Line: 12, Body: "why?"},
				{ID: id*10 + 1, Path: "b.go", Line: 3, Body: "typo"},
			},
		}
	}
	if reviewMember(on(1)) != reviewMember(on(99)) {
		t.Error("the same review didn't match across nodes")
	}
	// The order Forgejo happens to list the line comments in doesn't count.
	shuffled := on(1)
	shuffled.comments[0], shuffled.comments[1] = shuffled.comments[1], shuffled.comments[0]
	if reviewMember(shuffled) != reviewMember(on(1)) {
		t.Error("the listing order changed the member")
	}
	// Anything a person wrote does.
	changed := on(1)
	changed.comments[0].Body = "why not?"
	if reviewMember(changed) == reviewMember(on(1)) {
		t.Error("a different remark matched")
	}
	moved := on(1)
	moved.comments[0].Line = 13
	if reviewMember(moved) == reviewMember(on(1)) {
		t.Error("a different line matched")
	}

	// Drafts and team reviews are left alone.
	draft := on(1)
	draft.head.State = "PENDING"
	nobody := on(1)
	nobody.head.Reviewer = nil
	if submitted(draft) || submitted(nobody) {
		t.Error("a draft or a team review was taken for a submitted one")
	}
}

func TestReviewsSpreadWithTheirLineComments(t *testing.T) {
	s, st, f, _ := setup(t)
	s.opts.PullRequests, s.opts.Reviews = true, true
	for _, n := range []string{"dk", "de"} {
		f[n].branches["topic/change"] = true
	}
	f["se"].openPull("alice", "Add notes", "topic/change", "main")
	s.run(t)
	f["se"].reviewOn(1, "bob", "REQUEST_CHANGES", "Nearly.", [2]string{"lines.txt", "this line worries me"})
	s.run(t)

	want := "bob REQUEST_CHANGES Nearly. / lines.txt:1 this line worries me"
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reviewsOnPull(1); got != want {
			t.Fatalf("%s: %q", n, got)
		}
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}

	// A second review on a replica reaches the others; a review withdrawn
	// elsewhere goes everywhere. Neither is a conflict.
	f["de"].reviewOn(1, "carol", "APPROVED", "Looks good.")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reviewsOnPull(1); got == want {
			t.Errorf("%s didn't get the approval: %q", n, got)
		}
	}
	f["se"].mu.Lock()
	f["se"].reviews[1] = f["se"].reviews[1][1:] // bob's is withdrawn on se
	f["se"].mu.Unlock()
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reviewsOnPull(1); got != "carol APPROVED Looks good." {
			t.Errorf("%s after the withdrawal: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

// A node that won't take a review leaves it half-copied, which must never
// read as the reviewer withdrawing it everywhere.
func TestAReviewANodeWontTake(t *testing.T) {
	s, _, f, _ := setup(t)
	s.opts.PullRequests, s.opts.Reviews = true, true
	for _, n := range []string{"dk", "de"} {
		f[n].branches["topic/change"] = true
	}
	f["de"].refusesReview = "bob"
	f["se"].openPull("alice", "Add notes", "topic/change", "main")
	s.run(t)
	f["se"].reviewOn(1, "bob", "COMMENT", "A note.")
	for i := 0; i < 3; i++ {
		s.run(t)
		if got := f["dk"].reviewsOnPull(1); got != "bob COMMENT A note." {
			t.Fatalf("round %d: dk %q", i, got)
		}
		if got := f["se"].reviewsOnPull(1); got != "bob COMMENT A note." {
			t.Fatalf("round %d: se lost it: %q", i, got)
		}
	}
	f["de"].refusesReview = ""
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].reviewsOnPull(1); got != "bob COMMENT A note." {
			t.Errorf("%s: %q", n, got)
		}
	}
}
