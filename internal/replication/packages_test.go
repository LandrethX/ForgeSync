package replication

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// packagesSetup is three nodes that all have alice/demo, so alice is an
// owner whose packages ForgeSync brings together.
func packagesSetup(t *testing.T) (*Engine, *memStore, map[string]*gitNode, []store.RepositoryRecord) {
	t.Helper()
	nodes := map[string]*gitNode{"se": newGitNode(t), "dk": newGitNode(t), "de": newGitNode(t)}
	rec := store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"}
	var ns []Node
	for _, name := range []string{"se", "dk", "de"} {
		nodes[name].create("alice/demo")
		rec.Replicas = append(rec.Replicas, store.Replica{Node: name, Present: true})
		ns = append(ns, Node{Name: name, URL: nodes[name].srv.URL, User: testUser, Token: testToken,
			API: newFakeAPI(nodes[name])})
	}
	st := newMemStore(rec)
	e := NewEngine(ns, testGit(t), st, fixedHealth{"se": health.Healthy, "dk": health.Healthy, "de": health.Healthy},
		Options{Packages: true}, slog.New(slog.DiscardHandler))
	return e, st, nodes, []store.RepositoryRecord{rec}
}

var pkgHealthy = map[string]bool{"se": true, "dk": true, "de": true}

func TestAPackagePublishedAnywhereReachesEveryNode(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	nodes["se"].publish("alice", "generic", "demo-art", "1.0.0", "demo.bin", "the bytes")

	e.syncPackages(bg(), recs, pkgHealthy)

	for _, n := range []string{"se", "dk", "de"} {
		if got := nodes[n].packageList(); got != "generic demo-art 1.0.0 demo.bin=the bytes" {
			t.Fatalf("%s: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
	// A settled run publishes nothing.
	before := len(st.audit)
	e.syncPackages(bg(), recs, pkgHealthy)
	if len(st.audit) != before {
		t.Errorf("a settled run wrote: %v", st.audit[before:])
	}

	// One published on a replica travels the same way.
	nodes["de"].publish("alice", "generic", "demo-art", "1.1.0", "demo.bin", "newer bytes")
	e.syncPackages(bg(), recs, pkgHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := nodes[n].packageList(); !strings.Contains(got, "1.1.0 demo.bin=newer bytes") {
			t.Fatalf("%s: %q", n, got)
		}
	}
}

// maven lays a package out as a Maven repository, from the package's own
// name: the path is derived, not stored.
func TestAMavenPackageTravelsByItsLayout(t *testing.T) {
	e, _, nodes, recs := packagesSetup(t)
	nodes["se"].publish("alice", "maven", "org.demoscene:intro", "2.0", "intro-2.0.jar", "a jar")
	nodes["se"].publish("alice", "maven", "org.demoscene:intro", "2.0", "intro-2.0.pom", "a pom")

	e.syncPackages(bg(), recs, pkgHealthy)

	for _, n := range []string{"dk", "de"} {
		got := nodes[n].packageList()
		if !strings.Contains(got, "intro-2.0.jar=a jar") || !strings.Contains(got, "intro-2.0.pom=a pom") {
			t.Fatalf("%s: %q", n, got)
		}
	}
}

// A type ForgeSync can't publish is said so plainly, naming the type,
// rather than being left to look like nothing is wrong.
func TestATypeThatCantBeCarriedIsAConflict(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	nodes["se"].publish("alice", "npm", "demo", "1.0.0", "demo-1.0.0.tgz", "a tarball")

	e.syncPackages(bg(), recs, pkgHealthy)

	if got := nodes["dk"].packageList(); got != "" {
		t.Fatalf("dk got something: %q", got)
	}
	if len(st.found) == 0 || st.found[0].Kind != PackageTypeConflictKind {
		t.Fatalf("conflicts = %+v", st.found)
	}
	if st.found[0].Details["package_type"] != "npm" {
		t.Errorf("details = %v", st.found[0].Details)
	}
	// One conflict for the package, naming every node that hasn't got it,
	// rather than one per node or one per file.
	if len(st.found) != 1 {
		t.Fatalf("conflicts = %+v", st.found)
	}
	missing, _ := st.found[0].Details["missing"].([]string)
	if strings.Join(missing, ",") != "dk,de" {
		t.Errorf("missing = %v", missing)
	}
	// Every conflict belongs to a repository: a package has none of its
	// own, so it's recorded against the owner's. Without an id the
	// database refuses it outright.
	for _, c := range st.found {
		if c.RepositoryID != st.rec.ID {
			t.Fatalf("conflict has repository %q, want %q: %+v", c.RepositoryID, st.rec.ID, c)
		}
	}
}

// The owner's other repositories carry the same conflict, since that's
// where someone would look for it.
func TestAPackageConflictIsRecordedAgainstEveryRepository(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	recs = append(recs, store.RepositoryRecord{ID: "22222222-2222-2222-2222-222222222222",
		FullName: "alice/second", PrimaryNode: "se"})
	nodes["se"].publish("alice", "npm", "demo", "1.0.0", "demo-1.0.0.tgz", "a tarball")

	e.syncPackages(bg(), recs, pkgHealthy)

	ids := map[string]bool{}
	for _, c := range st.found {
		ids[c.RepositoryID] = true
	}
	if len(ids) != 2 || !ids[recs[0].ID] || !ids[recs[1].ID] {
		t.Fatalf("recorded against %v", ids)
	}
}

// Deleted after every node had it: deleted everywhere. A version whose
// files have all gone is removed whole, which is how a type with no
// endpoint for one file can be cleared at all.
func TestAPackageDeletedEverywhereIsDeletedEverywhere(t *testing.T) {
	e, _, nodes, recs := packagesSetup(t)
	nodes["se"].publish("alice", "generic", "demo-art", "1.0.0", "demo.bin", "the bytes")
	e.syncPackages(bg(), recs, pkgHealthy)

	if err := e.nodes["dk"].API.DeletePackage(bg(), "alice", "generic", "demo-art", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	e.syncPackages(bg(), recs, pkgHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := nodes[n].packageList(); got != "" {
			t.Fatalf("%s still has %q", n, got)
		}
	}
}

// Two nodes publishing different content under one name is a conflict:
// a published file doesn't change, so one of them is not what it says.
func TestTwoFilesOfOneNameAreAConflict(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	nodes["se"].publish("alice", "generic", "demo-art", "1.0.0", "demo.bin", "what se has")
	nodes["dk"].publish("alice", "generic", "demo-art", "1.0.0", "demo.bin", "what dk has")

	e.syncPackages(bg(), recs, pkgHealthy)

	if got := nodes["se"].packageList(); got != "generic demo-art 1.0.0 demo.bin=what se has" {
		t.Errorf("se was written over: %q", got)
	}
	if got := nodes["dk"].packageList(); got != "generic demo-art 1.0.0 demo.bin=what dk has" {
		t.Errorf("dk was written over: %q", got)
	}
	// The third node is given neither: ForgeSync doesn't pick one.
	if got := nodes["de"].packageList(); got != "" {
		t.Errorf("de was given one of them: %q", got)
	}
	var kinds []string
	for _, c := range st.found {
		kinds = append(kinds, c.Kind+" "+c.Ref)
	}
	if len(st.found) == 0 || st.found[0].Kind != PackageConflictKind {
		t.Fatalf("conflicts = %v", kinds)
	}
}

// nuget, rubygems and helm are each one file uploaded to a fixed endpoint
// and read back from somewhere else, so the two paths have to agree for a
// package to travel at all.
func TestTheSingleFileRegistriesTravel(t *testing.T) {
	cases := []struct {
		typ, name, version, file string
		body                     []byte
	}{
		{"nuget", "ForgeSyncDemo", "1.0.0", "forgesyncdemo.1.0.0.nupkg", nupkgFor("ForgeSyncDemo", "1.0.0")},
		{"rubygems", "demo", "1.0.0", "demo-1.0.0.gem", gemFor("demo", "1.0.0")},
		{"helm", "demo", "1.0.0", "demo-1.0.0.tgz", chartFor("demo", "1.0.0")},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			e, st, nodes, recs := packagesSetup(t)
			nodes["se"].upload("alice", tc.typ, tc.body)

			e.syncPackages(bg(), recs, pkgHealthy)

			for _, n := range []string{"se", "dk", "de"} {
				got := nodes[n].fileNames("alice", tc.typ, tc.name, tc.version)
				if !slices.Contains(got, tc.file) {
					t.Fatalf("%s holds %v, want %s among them", n, got, tc.file)
				}
				if !bytes.Equal(nodes[n].fileBody("alice", tc.typ, tc.name, tc.version, tc.file), tc.body) {
					t.Errorf("%s: the file that arrived is not the one that was published", n)
				}
			}
			if len(st.found) != 0 {
				t.Errorf("conflicts: %+v", st.found)
			}
			// A settled run publishes nothing: what arrived compares equal
			// to what was sent, derived files and all.
			before := len(st.audit)
			e.syncPackages(bg(), recs, pkgHealthy)
			if len(st.audit) != before {
				t.Errorf("a settled run wrote: %v", st.audit[before:])
			}
		})
	}
}

// Forgejo extracts a .nuspec from every .nupkg it is given, so a node that
// has never been sent one still lists it. ForgeSync has to leave it alone:
// publishing the .nupkg recreates it, and publishing it on its own would
// be sending a fragment to the endpoint that takes whole packages.
func TestTheNuspecForgejoMakesIsNotCarried(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	nodes["se"].upload("alice", "nuget", nupkgFor("ForgeSyncDemo", "1.0.0"))
	if got := nodes["se"].fileNames("alice", "nuget", "ForgeSyncDemo", "1.0.0"); len(got) != 2 {
		t.Fatalf("the node did not make a nuspec of its own: %v", got)
	}

	e.syncPackages(bg(), recs, pkgHealthy)

	for _, n := range []string{"dk", "de"} {
		got := nodes[n].fileNames("alice", "nuget", "ForgeSyncDemo", "1.0.0")
		want := []string{"forgesyncdemo.1.0.0.nupkg", "forgesyncdemo.nuspec"}
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", n, got, want)
		}
	}
	// One publish each, of the package itself; the nuspec is never one.
	for _, a := range st.audit {
		if strings.Contains(a, ".nuspec") {
			t.Errorf("ForgeSync acted on a file the node made for itself: %s", a)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

// A file only one node has is a package one node has: removing the node
// that has it removes the version whole, which is the only way for a type
// whose registry has no endpoint for one file.
func TestASingleFileVersionGoesWhole(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	for _, n := range []string{"se", "dk", "de"} {
		nodes[n].upload("alice", "helm", chartFor("demo", "1.0.0"))
	}
	// Everyone has it, so it settles into the base.
	e.syncPackages(bg(), recs, pkgHealthy)
	if got := st.packageBase["alice"]; !strings.Contains(got, "helm|demo|1.0.0|demo-1.0.0.tgz") {
		t.Fatalf("base = %q", got)
	}
	// Deleted on one node, it goes everywhere.
	nodes["se"].dropPackage("alice", "helm", "demo", "1.0.0")
	e.syncPackages(bg(), recs, pkgHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		if got := nodes[n].fileNames("alice", "helm", "demo", "1.0.0"); len(got) != 0 {
			t.Errorf("%s still holds %v", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}

// A node that could not be read this run is not in the set, so the base
// must not settle without it. When it comes back, everything it hasn't
// got would otherwise read as someone's deletion and be taken off every
// other node: here, published package files deleted everywhere because
// one node was down while they were published.
func TestTheBaseWaitsForANodeThatCouldNotBeRead(t *testing.T) {
	e, st, nodes, recs := packagesSetup(t)
	nodes["se"].publish("alice", "generic", "demo-art", "1.0.0", "demo.bin", "the bytes")
	e.syncPackages(bg(), recs, pkgHealthy)
	if !strings.Contains(st.packageBase["alice"], "demo.bin") {
		t.Fatalf("base = %q", st.packageBase["alice"])
	}

	// de goes away, and something is published while it is gone.
	away := map[string]bool{"se": true, "dk": true, "de": false}
	nodes["se"].publish("alice", "generic", "demo-art", "2.0.0", "demo.bin", "newer bytes")
	e.syncPackages(bg(), recs, away)
	if got := nodes["dk"].packageList(); !strings.Contains(got, "2.0.0 demo.bin=newer bytes") {
		t.Errorf("dk didn't get it while de was away: %q", got)
	}
	if strings.Contains(st.packageBase["alice"], "2.0.0") {
		t.Fatalf("the base settled without de: %q", st.packageBase["alice"])
	}

	// de comes back. It hasn't got the new version, and that is not a
	// deletion: it is given the file, and nothing is taken away.
	e.syncPackages(bg(), recs, pkgHealthy)
	for _, n := range []string{"se", "dk", "de"} {
		got := nodes[n].packageList()
		if !strings.Contains(got, "1.0.0 demo.bin=the bytes") {
			t.Errorf("%s lost the first version: %q", n, got)
		}
		if !strings.Contains(got, "2.0.0 demo.bin=newer bytes") {
			t.Errorf("%s hasn't got the second version: %q", n, got)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}
}
