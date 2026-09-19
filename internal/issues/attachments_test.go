package issues

import (
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
)

func TestAttachmentsAreMatchedBySizeAndName(t *testing.T) {
	list := []forgejo.Attachment{
		{ID: 1, Name: "note.txt", Size: 14, Type: "attachment"},
		{ID: 2, Name: "shot.png", Size: 9000, Type: ""},       // older Forgejo leaves it empty
		{ID: 3, Name: "elsewhere", Size: 0, Type: "external"}, // only a link
	}
	got := attachmentsOn(list)
	if len(got) != 2 || got["14:note.txt"].ID != 1 || got["9000:shot.png"].ID != 2 {
		t.Fatalf("attachments = %+v", got)
	}
	if _, external := got["0:elsewhere"]; external {
		t.Error("an external attachment took part")
	}
	if attachmentName("14:note.txt") != "note.txt" {
		t.Errorf("name = %q", attachmentName("14:note.txt"))
	}
	// A name with a colon in it survives the round trip.
	if attachmentName(attachmentMember(forgejo.Attachment{Name: "a:b.txt", Size: 3})) != "a:b.txt" {
		t.Error("a colon in the name")
	}
}

func TestAttachmentsSpreadBothWays(t *testing.T) {
	s, st, f, _ := setup(t)
	s.opts.Attachments = true
	f["se"].open("alice", "with a file")
	s.run(t)
	f["se"].attach(1, "note.txt", "hello from se")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].filesOn(1); got != "note.txt=hello from se" {
			t.Fatalf("%s: %q", n, got)
		}
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a settled run wrote %d times", n)
	}

	// One added on a replica and another taken off elsewhere at once: both,
	// and no conflict.
	f["dk"].attach(1, "shot.png", "PNG bytes")
	f["de"].detach(1, "note.txt")
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].filesOn(1); got != "shot.png=PNG bytes" {
			t.Errorf("%s: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

func TestCommentAttachmentsSpread(t *testing.T) {
	s, _, f, _ := setup(t)
	s.opts.Attachments = true
	f["se"].open("alice", "with a comment")
	id := f["se"].say("bob", 1, "see the log").ID
	s.run(t)
	f["se"].mu.Lock()
	f["se"].nextFile++
	f["se"].putFile(true, id, &fakeFile{id: f["se"].nextFile, name: "run.log", content: []byte("it failed")})
	f["se"].mu.Unlock()
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		ids := f[n].commentIDs(1)
		if len(ids) != 1 {
			t.Fatalf("%s has %d comments", n, len(ids))
		}
		if got := f[n].commentFilesOn(ids[0]); got != "run.log=it failed" {
			t.Errorf("%s comment: %q", n, got)
		}
	}
}

// A node that won't take a file leaves it half-copied. That must never be
// read as someone deleting it everywhere; it's tried again until it lands.
func TestAFileANodeWontTake(t *testing.T) {
	s, st, f, _ := setup(t)
	s.opts.Attachments = true
	f["de"].rejects = "virus.exe"
	f["se"].open("alice", "with a file")
	s.run(t)
	f["se"].attach(1, "virus.exe", "MZ")
	for i := 0; i < 3; i++ {
		s.run(t)
		if got, other := f["se"].filesOn(1), f["dk"].filesOn(1); got != "virus.exe=MZ" || other != "virus.exe=MZ" {
			t.Fatalf("round %d: se %q dk %q", i, got, other)
		}
		if got := f["de"].filesOn(1); got != "" {
			t.Fatalf("round %d: de took it after all: %q", i, got)
		}
	}
	// Once the node will take it, it lands there too.
	f["de"].rejects = ""
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		if got := f[n].filesOn(1); got != "virus.exe=MZ" {
			t.Errorf("%s: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

// A file over the limit is left where it is, and doesn't disturb the rest.
func TestAFileTooBigToCarry(t *testing.T) {
	s, _, f, _ := setup(t)
	s.opts.Attachments = true
	s.opts.AttachmentMax = 8
	f["se"].open("alice", "with files")
	s.run(t)
	f["se"].attach(1, "small.txt", "tiny")
	f["se"].attach(1, "big.bin", strings.Repeat("x", 64))
	s.run(t)
	for _, n := range []string{"dk", "de"} {
		if got := f[n].filesOn(1); got != "small.txt=tiny" {
			t.Errorf("%s: %q", n, got)
		}
	}
	// The big one stays on se: nothing deletes it there.
	if got := f["se"].filesOn(1); !strings.Contains(got, "big.bin") || !strings.Contains(got, "small.txt") {
		t.Errorf("se: %q", got)
	}
}
