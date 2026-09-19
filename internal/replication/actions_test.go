package replication

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// actionsSetup is a primary and two replicas that all have the repository,
// each with its own Actions variables and secrets.
func actionsSetup(t *testing.T) (*Engine, *memStore, map[string]*fakeAPI) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	var nodes []Node
	for _, n := range []string{"se", "dk", "de"} {
		rec.Replicas = append(rec.Replicas, store.Replica{Node: n, Present: true})
		apis[n].vars = map[string]string{}
		nodes = append(nodes, Node{Name: n, URL: "http://" + n, User: testUser, Token: testToken, API: apis[n]})
	}
	st := newMemStore(rec)
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{Actions: true}, slog.New(slog.DiscardHandler))
	return e, st, apis
}

// vars is a node's variables as "NAME=value", in order.
func vars(a *fakeAPI) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for name, data := range a.vars {
		out = append(out, name+"="+data)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

var allHealthy = map[string]bool{"se": true, "dk": true, "de": true}

func TestVariablesSpreadBothWays(t *testing.T) {
	e, st, apis := actionsSetup(t)
	ctx := context.Background()
	rec, _ := st.Repository(ctx, st.rec.ID)

	// Set on the primary: it reaches the replicas.
	apis["se"].vars["REGION"] = "europe"
	e.syncActions(ctx, rec, allHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := vars(apis[n]); got != "REGION=europe" {
			t.Fatalf("%s: %q", n, got)
		}
	}
	rec, _ = st.Repository(ctx, st.rec.ID)
	if rec.BaseActionVariables == "" {
		t.Fatal("nothing recorded as agreed")
	}

	// A settled run writes nothing.
	for _, a := range apis {
		a.calls = nil
	}
	e.syncActions(ctx, rec, allHealthy)
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("a settled run wrote on %s: %v", n, a.calls)
		}
	}

	// Set on a replica: it reaches the others just the same.
	apis["de"].vars["TIER"] = "test"
	e.syncActions(ctx, rec, allHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := vars(apis[n]); got != "REGION=europe,TIER=test" {
			t.Fatalf("%s: %q", n, got)
		}
	}

	// Removed anywhere: it goes everywhere.
	rec, _ = st.Repository(ctx, st.rec.ID)
	delete(apis["dk"].vars, "REGION")
	e.syncActions(ctx, rec, allHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := vars(apis[n]); got != "TIER=test" {
			t.Fatalf("%s after the removal: %q", n, got)
		}
	}
}

// A changed value is one member leaving and another arriving. It must be
// written as an update, so a workflow never catches the variable missing.
func TestChangedVariableIsUpdatedNotRecreated(t *testing.T) {
	e, st, apis := actionsSetup(t)
	ctx := context.Background()
	rec, _ := st.Repository(ctx, st.rec.ID)
	for _, n := range []string{"se", "dk", "de"} {
		apis[n].vars["REGION"] = "europe"
	}
	e.syncActions(ctx, rec, allHealthy)
	rec, _ = st.Repository(ctx, st.rec.ID)

	apis["se"].vars["REGION"] = "nordics"
	for _, a := range apis {
		a.calls = nil
	}
	e.syncActions(ctx, rec, allHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := vars(apis[n]); got != "REGION=nordics" {
			t.Fatalf("%s: %q", n, got)
		}
	}
	for _, n := range []string{"dk", "de"} {
		if got := strings.Join(apis[n].calls, ","); got != "update variable REGION" {
			t.Errorf("%s: %q, wanted one update", n, got)
		}
	}
}

// Two nodes changing the same variable to different values is a conflict:
// neither is written over.
func TestVariableChangedTwoWaysIsLeftAlone(t *testing.T) {
	e, st, apis := actionsSetup(t)
	ctx := context.Background()
	rec, _ := st.Repository(ctx, st.rec.ID)
	for _, n := range []string{"se", "dk", "de"} {
		apis[n].vars["REGION"] = "europe"
	}
	e.syncActions(ctx, rec, allHealthy)
	rec, _ = st.Repository(ctx, st.rec.ID)

	apis["se"].vars["REGION"] = "nordics"
	apis["dk"].vars["REGION"] = "baltics"
	e.syncActions(ctx, rec, allHealthy)
	if got := vars(apis["se"]); got != "REGION=nordics" {
		t.Errorf("se: %q", got)
	}
	if got := vars(apis["dk"]); got != "REGION=baltics" {
		t.Errorf("dk: %q", got)
	}
	if got := vars(apis["de"]); got != "REGION=europe" {
		t.Errorf("de: %q", got)
	}
	if len(st.found) != 1 || st.found[0].Kind != VariableConflictKind || st.found[0].Ref != "REGION" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	values, _ := st.found[0].Details["values"].(map[string]any)
	if values["se"] != "nordics" || values["dk"] != "baltics" || values["de"] != "europe" {
		t.Errorf("values = %v", values)
	}

	// Settled by a person: the agreed value spreads and the conflict goes.
	apis["dk"].vars["REGION"] = "nordics"
	rec, _ = st.Repository(ctx, st.rec.ID)
	e.syncActions(ctx, rec, allHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := vars(apis[n]); got != "REGION=nordics" {
			t.Fatalf("%s after settling: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Fatalf("still conflicting: %+v", st.found)
	}
}

// Forgejo never gives a secret's value back, so a node missing one is a
// conflict for a person and nothing is written anywhere.
func TestSecretMissingOnANodeIsAConflict(t *testing.T) {
	e, st, apis := actionsSetup(t)
	ctx := context.Background()
	rec, _ := st.Repository(ctx, st.rec.ID)
	apis["se"].secrets = []string{"DEPLOY_KEY", "TOKEN"}
	apis["dk"].secrets = []string{"TOKEN"}
	apis["de"].secrets = []string{"TOKEN"}

	e.syncActions(ctx, rec, allHealthy)
	if len(st.found) != 1 {
		t.Fatalf("conflicts = %v", st.found)
	}
	c := st.found[0]
	if c.Kind != SecretConflictKind || c.Ref != "DEPLOY_KEY" {
		t.Fatalf("conflict = %+v", c)
	}
	missing, _ := c.Details["missing"].([]string)
	if got := strings.Join(missing, ","); got != "dk,de" {
		t.Errorf("missing = %q", got)
	}
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("%s was written to: %v", n, a.calls)
		}
	}

	// Once it's set there too, the conflict clears.
	apis["dk"].secrets = append(apis["dk"].secrets, "DEPLOY_KEY")
	apis["de"].secrets = append(apis["de"].secrets, "DEPLOY_KEY")
	e.syncActions(ctx, rec, allHealthy)
	if len(st.found) != 0 {
		t.Fatalf("still conflicting: %v", st.found)
	}
}

// An unhealthy node is left out entirely: its variables are neither read
// nor treated as someone having removed them.
func TestUnhealthyNodeDoesNotRemoveVariables(t *testing.T) {
	e, st, apis := actionsSetup(t)
	ctx := context.Background()
	rec, _ := st.Repository(ctx, st.rec.ID)
	for _, n := range []string{"se", "dk", "de"} {
		apis[n].vars["REGION"] = "europe"
	}
	e.syncActions(ctx, rec, allHealthy)
	rec, _ = st.Repository(ctx, st.rec.ID)

	for _, a := range apis {
		a.calls = nil
	}
	e.syncActions(ctx, rec, map[string]bool{"se": true, "dk": true})
	for _, n := range []string{"se", "dk", "de"} {
		if got := vars(apis[n]); got != "REGION=europe" {
			t.Fatalf("%s: %q", n, got)
		}
	}
}

// Actions can be turned off for a repository on a node, which is that
// node's decision (has_actions isn't replicated). The endpoints then
// answer 404, which is an answer -- "there are none here" -- not a
// failure worth a warning every round.
func TestANodeWithActionsOffIsSkippedQuietly(t *testing.T) {
	e, st, apis := actionsSetup(t)
	ctx := context.Background()
	rec, _ := st.Repository(ctx, st.rec.ID)
	apis["se"].vars["REGION"] = "europe"
	apis["de"].actionsOff = true

	e.syncActions(ctx, rec, allHealthy)

	if got := vars(apis["dk"]); got != "REGION=europe" {
		t.Errorf("dk: %q", got)
	}
	if got := vars(apis["de"]); got != "" {
		t.Errorf("de was written to although Actions are off there: %q", got)
	}
	if len(st.found) != 0 {
		t.Errorf("a node with Actions off became a conflict: %+v", st.found)
	}
}
