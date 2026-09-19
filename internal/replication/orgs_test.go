package replication

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// orgSetup is three nodes, one of which has the organization.
func orgSetup(t *testing.T) (*Engine, *memStore, map[string]*fakeAPI, []store.RepositoryRecord) {
	t.Helper()
	apis := map[string]*fakeAPI{"se": newFakeAPI(nil), "dk": newFakeAPI(nil), "de": newFakeAPI(nil)}
	var nodes []Node
	for _, n := range []string{"se", "dk", "de"} {
		a := apis[n]
		a.orgState = map[string]*fakeOrg{}
		a.orgs = map[string]bool{}
		for _, login := range []string{"alice", "bob"} {
			a.users[login] = forgejo.User{Login: login, SourceID: 1, LoginName: login + "-sub"}
		}
		nodes = append(nodes, Node{Name: n, URL: "http://" + n, User: testUser, Token: testToken,
			API: a, SceneIDSourceID: 1})
	}
	// se has "demoscene": alice owns it, bob is in a "coders" team.
	se := apis["se"]
	se.orgs["demoscene"] = true
	se.nextTeam = 1
	se.orgState["demoscene"] = &fakeOrg{
		org: forgejo.Org{Name: "demoscene", FullName: "The Demoscene", Description: "Keeping it alive", Visibility: "public"},
		teams: map[string]*forgejo.Team{
			"Owners": {ID: 1, Name: "Owners", Permission: "owner"},
			"coders": {ID: 2, Name: "coders", Permission: "write", Units: []string{"repo.code"}},
		},
		members: map[int64]map[string]bool{1: {"alice": true}, 2: {"bob": true}},
	}
	se.nextTeam = 2
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "demoscene/intro", PrimaryNode: "se"}
	st := newMemStore(rec)
	e := NewEngine(nodes, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{Organizations: true}, slog.New(slog.DiscardHandler))
	return e, st, apis, []store.RepositoryRecord{rec}
}

func orgOn(a *fakeAPI, name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.orgState[name]
	if o == nil {
		return ""
	}
	var teams []string
	for tn, t := range o.teams {
		var who []string
		for login := range o.members[t.ID] {
			who = append(who, login)
		}
		slices.Sort(who)
		teams = append(teams, tn+":"+t.Permission+"("+strings.Join(who, ",")+")")
	}
	slices.Sort(teams)
	return o.org.FullName + " | " + o.org.Description + " | " + strings.Join(teams, " ")
}

func TestOrganizationsAreCreatedAndKeptTheSame(t *testing.T) {
	e, st, apis, recs := orgSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}

	e.syncOrgs(ctx, recs, healthy)
	want := orgOn(apis["se"], "demoscene")
	for _, n := range []string{"dk", "de"} {
		if got := orgOn(apis[n], "demoscene"); got != want {
			t.Fatalf("%s:\n got %q\nwant %q", n, got, want)
		}
	}
	rec, _ := st.Org(ctx, "demoscene")
	if rec.BaseFields["description"] != "Keeping it alive" || !strings.Contains(rec.BaseTeams, "coders:write") ||
		!strings.Contains(rec.BaseMembers, "coders:bob") {
		t.Fatalf("what was agreed = %+v", rec)
	}
	for _, a := range apis {
		a.calls = nil
	}
	e.syncOrgs(ctx, recs, healthy)
	for n, a := range apis {
		if len(a.calls) != 0 {
			t.Errorf("a settled run wrote on %s: %v", n, a.calls)
		}
	}

	// A field changed on a replica reaches the others.
	apis["de"].orgState["demoscene"].org.Description = "Still alive"
	e.syncOrgs(ctx, recs, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := apis[n].orgState["demoscene"].org.Description; got != "Still alive" {
			t.Errorf("%s description = %q", n, got)
		}
	}

	// Someone added to a team on a replica reaches the others.
	apis["dk"].mu.Lock()
	dk := apis["dk"].orgState["demoscene"]
	dk.members[dk.teams["coders"].ID]["alice"] = true
	apis["dk"].mu.Unlock()
	e.syncOrgs(ctx, recs, healthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := orgOn(apis[n], "demoscene"); !strings.Contains(got, "coders:write(alice,bob)") {
			t.Errorf("%s: %q", n, got)
		}
	}
}

// Two nodes given different descriptions: a conflict for a person, and
// nothing is written.
func TestOrganizationFieldsCanConflict(t *testing.T) {
	e, st, apis, recs := orgSetup(t)
	ctx := context.Background()
	healthy := map[string]bool{"se": true, "dk": true, "de": true}
	e.syncOrgs(ctx, recs, healthy)

	apis["se"].orgState["demoscene"].org.Description = "One thing"
	apis["de"].orgState["demoscene"].org.Description = "Another thing"
	e.syncOrgs(ctx, recs, healthy)

	if len(st.found) != 1 || st.found[0].Kind != OrgConflictKind || st.found[0].Ref != "demoscene description" {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if vals := st.found[0].Details["values"].(map[string]string); vals["se"] != "One thing" || vals["de"] != "Another thing" {
		t.Errorf("values = %v", vals)
	}
	// Nothing was written, so nobody's wording was lost.
	if apis["se"].orgState["demoscene"].org.Description != "One thing" ||
		apis["de"].orgState["demoscene"].org.Description != "Another thing" {
		t.Error("a description was overwritten")
	}
	rec, _ := st.Org(ctx, "demoscene")
	if rec.BaseFields["description"] != "Keeping it alive" {
		t.Errorf("the base moved to %q", rec.BaseFields["description"])
	}
}
