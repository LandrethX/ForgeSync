package replication

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// guardSetup is a primary and two replicas that all have the repository.
func guardSetup(t *testing.T, opts Options) (*Engine, *memStore, map[string]*fakeAPI, store.RepositoryRecord) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	var nodes []Node
	for _, n := range []string{"se", "dk", "de"} {
		rec.Replicas = append(rec.Replicas, store.Replica{Node: n, Present: true})
		apis[n].rules = map[string]forgejo.BranchProtection{}
		nodes = append(nodes, Node{Name: n, URL: "http://" + n, User: testUser, Token: testToken, API: apis[n]})
	}
	st := newMemStore(rec)
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		opts, slog.New(slog.DiscardHandler))
	return e, st, apis, rec
}

func rulesOn(a *fakeAPI) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for name := range a.rules {
		out = append(out, name)
	}
	sortStringsForTest(out)
	return strings.Join(out, ",")
}

func sortStringsForTest(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// The replicas are guarded and the primary isn't, and a guard someone
// removes or weakens is put back. This is Phase 0's p02.
func TestTheReplicasAreGuardedAndStayGuarded(t *testing.T) {
	e, _, apis, rec := guardSetup(t, Options{ProtectReplicas: true})
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}

	e.protectReplicas(ctx, rec, e.nodes["se"], healthy)
	if got := rulesOn(apis["se"]); got != "" {
		t.Errorf("the primary was guarded: %q", got)
	}
	for _, n := range []string{"dk", "de"} {
		if got := rulesOn(apis[n]); got != guardRule {
			t.Fatalf("%s: %q", n, got)
		}
		r := apis[n].rules[guardRule]
		if !guardIsRight(r) {
			t.Errorf("%s: the guard isn't what it should be: %+v", n, r)
		}
		// The service account has to be named, or ForgeSync locks itself
		// out: being a site admin doesn't get past a push whitelist.
		if len(r.PushWhitelistUsernames) != 1 || r.PushWhitelistUsernames[0] != testUser {
			t.Errorf("%s: whitelist = %v, want the service account", n, r.PushWhitelistUsernames)
		}
	}
	for _, a := range apis {
		a.calls = nil
	}
	e.protectReplicas(ctx, rec, e.nodes["se"], healthy)
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("a settled run wrote on %s: %v", n, a.calls)
		}
	}

	// The owner deletes it, as Phase 0 found they can.
	apis["dk"].mu.Lock()
	delete(apis["dk"].rules, guardRule)
	apis["dk"].mu.Unlock()
	// And weakens it on the other replica.
	apis["de"].mu.Lock()
	weak := apis["de"].rules[guardRule]
	weak.EnablePushWhitelist = false
	apis["de"].rules[guardRule] = weak
	apis["de"].mu.Unlock()

	e.protectReplicas(ctx, rec, e.nodes["se"], healthy)
	for _, n := range []string{"dk", "de"} {
		if !guardIsRight(apis[n].rules[guardRule]) {
			t.Errorf("%s: the guard wasn't put back: %+v", n, apis[n].rules[guardRule])
		}
	}
}

// Forgejo won't let even a site admin rewrite or delete a protected
// branch, so ForgeSync lifts its own guard for those writes and puts it
// back.
func TestTheGuardIsLiftedForAWriteAndPutBack(t *testing.T) {
	e, _, apis, rec := guardSetup(t, Options{ProtectReplicas: true})
	ctx := context.Background()
	e.protectReplicas(ctx, rec, e.nodes["se"], map[string]bool{"se": true, "dk": true, "de": true})

	var guardedDuringWrite bool
	err := e.withoutGuard(ctx, rec, e.nodes["dk"], func() error {
		apis["dk"].mu.Lock()
		_, guardedDuringWrite = apis["dk"].rules[guardRule]
		apis["dk"].mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if guardedDuringWrite {
		t.Error("the guard was still on while ForgeSync wrote")
	}
	if !guardIsRight(apis["dk"].rules[guardRule]) {
		t.Error("the guard wasn't put back")
	}

	// It goes back even when the write fails.
	want := errors.New("push rejected")
	if err := e.withoutGuard(ctx, rec, e.nodes["dk"], func() error { return want }); !errors.Is(err, want) {
		t.Errorf("err = %v", err)
	}
	if !guardIsRight(apis["dk"].rules[guardRule]) {
		t.Error("the guard wasn't put back after a failed write")
	}
}

// The owner's own rules travel; ForgeSync's guard doesn't.
func TestTheOwnersRulesTravelAndTheGuardDoesNot(t *testing.T) {
	e, st, apis, rec := guardSetup(t, Options{ProtectReplicas: true, BranchProtection: true})
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	e.protectReplicas(ctx, rec, e.nodes["se"], healthy)

	// The owner protects main on the primary.
	apis["se"].rules["main"] = forgejo.BranchProtection{RuleName: "main", RequiredApprovals: 2, ApplyToAdmins: true}
	e.syncProtection(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		r, ok := apis[n].rules["main"]
		if !ok || r.RequiredApprovals != 2 {
			t.Fatalf("%s: main = %+v", n, r)
		}
	}
	if got := rulesOn(apis["se"]); got != "main" {
		t.Errorf("the guard reached the primary: %q", got)
	}
	rec, _ = st.Repository(ctx, rec.ID)
	if !strings.HasPrefix(rec.BaseProtection, "main:") {
		t.Fatalf("what was agreed = %q", rec.BaseProtection)
	}

	// Changed on a replica: an edit everywhere, never a gap in cover.
	apis["de"].mu.Lock()
	changed := apis["de"].rules["main"]
	changed.RequiredApprovals = 1
	apis["de"].rules["main"] = changed
	apis["de"].mu.Unlock()
	for _, a := range apis {
		a.calls = nil
	}
	e.syncProtection(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := apis[n].rules["main"].RequiredApprovals; got != 1 {
			t.Errorf("%s: approvals = %d", n, got)
		}
		if strings.Contains(strings.Join(apis[n].calls, " "), "unprotect main") {
			t.Errorf("%s left main unprotected on the way: %v", n, apis[n].calls)
		}
	}

	// Removed on a replica: removed everywhere, and the guard stays.
	rec, _ = st.Repository(ctx, rec.ID)
	apis["dk"].mu.Lock()
	delete(apis["dk"].rules, "main")
	apis["dk"].mu.Unlock()
	e.syncProtection(ctx, rec, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if _, ok := apis[n].rules["main"]; ok {
			t.Errorf("%s still has main", n)
		}
	}
	for _, n := range []string{"dk", "de"} {
		if _, ok := apis[n].rules[guardRule]; !ok {
			t.Errorf("%s lost its guard", n)
		}
	}
}

// Forgejo takes the push whitelist but never gives it back, so a whitelist
// someone has emptied only shows up as ForgeSync's push being refused.
// That is what makes it write the guard again.
func TestAPushTheGuardRefusesWritesItAgain(t *testing.T) {
	e, _, apis, rec := guardSetup(t, Options{ProtectReplicas: true})
	ctx := context.Background()
	e.protectReplicas(ctx, rec, e.nodes["se"], map[string]bool{"se": true, "dk": true, "de": true})

	// Someone empties the whitelist. Everything that can be read still
	// looks right, so the round-by-round check sees nothing wrong.
	apis["dk"].mu.Lock()
	emptied := apis["dk"].rules[guardRule]
	emptied.PushWhitelistUsernames = nil
	apis["dk"].rules[guardRule] = emptied
	apis["dk"].mu.Unlock()
	if !guardIsRight(apis["dk"].rules[guardRule]) {
		t.Fatal("this test is about the part that can't be seen")
	}
	e.protectReplicas(ctx, rec, e.nodes["se"], map[string]bool{"se": true, "dk": true, "de": true})
	if len(apis["dk"].rules[guardRule].PushWhitelistUsernames) != 0 {
		t.Fatal("the round-by-round check repaired it; this test no longer covers what it means to")
	}

	// A refused push is the only signal there is.
	e.reassertGuard(ctx, rec, e.nodes["dk"])
	if got := apis["dk"].rules[guardRule].PushWhitelistUsernames; len(got) != 1 || got[0] != testUser {
		t.Errorf("whitelist = %v, want the service account back", got)
	}
}
