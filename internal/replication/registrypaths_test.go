package replication

import (
	"net/http"
	"testing"
)

// What can travel is decided by the registry protocol, so these are the
// paths as Forgejo v16 actually routes them. Getting one wrong is a
// package fetched from nowhere or published into nowhere, which is the
// sort of thing a test should hold still.
func TestRegistryPaths(t *testing.T) {
	cases := map[string]struct {
		file            packageFile
		fetch           string
		method, publish string
		carried         bool
	}{
		"generic fetches and publishes in one place": {
			file:    packageFile{Type: "generic", Name: "demo", Version: "1.0", File: "demo.zip"},
			fetch:   "/api/packages/scene/generic/demo/1.0/demo.zip",
			method:  http.MethodPut,
			publish: "/api/packages/scene/generic/demo/1.0/demo.zip",
			carried: true,
		},
		"maven lays the group out as directories": {
			file:    packageFile{Type: "maven", Name: "org.scene:demo", Version: "1.0", File: "demo-1.0.jar"},
			fetch:   "/api/packages/scene/maven/org/scene/demo/1.0/demo-1.0.jar",
			method:  http.MethodPut,
			publish: "/api/packages/scene/maven/org/scene/demo/1.0/demo-1.0.jar",
			carried: true,
		},
		"nuget is read by name and version, written to one endpoint": {
			file:    packageFile{Type: "nuget", Name: "Demo", Version: "1.0.0", File: "Demo.1.0.0.nupkg"},
			fetch:   "/api/packages/scene/nuget/package/Demo/1.0.0/Demo.1.0.0.nupkg",
			method:  http.MethodPut,
			publish: "/api/packages/scene/nuget/",
			carried: true,
		},
		"rubygems is read by file name and posted": {
			file:    packageFile{Type: "rubygems", Name: "demo", Version: "1.0", File: "demo-1.0.gem"},
			fetch:   "/api/packages/scene/rubygems/gems/demo-1.0.gem",
			method:  http.MethodPost,
			publish: "/api/packages/scene/rubygems/api/v1/gems/",
			carried: true,
		},
		"helm is read by file name and posted to charts": {
			file:    packageFile{Type: "helm", Name: "demo", Version: "1.0", File: "demo-1.0.tgz"},
			fetch:   "/api/packages/scene/helm/demo-1.0.tgz",
			method:  http.MethodPost,
			publish: "/api/packages/scene/helm/api/charts",
			carried: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := registryFetch("scene", tc.file)
			if !ok || got != tc.fetch {
				t.Errorf("fetch = %q (ok %v), want %q", got, ok, tc.fetch)
			}
			method, path, ok := registryPublish("scene", tc.file)
			if !ok || path != tc.publish || method != tc.method {
				t.Errorf("publish = %s %q (ok %v), want %s %q", method, path, ok, tc.method, tc.publish)
			}
			if packageTypeCarried(tc.file.Type) != tc.carried {
				t.Errorf("carried = %v, want %v", !tc.carried, tc.carried)
			}
		})
	}
}

// A type whose upload wraps the file in something, or needs metadata the
// package API does not report, stays out: half a package is worse than
// none, and the conflict that names the type is the honest answer.
func TestTypesThatStayOut(t *testing.T) {
	for _, typ := range []string{"npm", "pypi", "composer", "container", "debian", "rpm", "alpine", "conan", "go", "swift", "vagrant", "cran", "conda", ""} {
		if packageTypeCarried(typ) {
			t.Errorf("%q is claimed to be carried, but nothing publishes it", typ)
		}
	}
}

// Some files in a package version were made by the node, not sent to it:
// maven builds its own metadata out of what it holds, and Forgejo
// extracts a .nuspec from every .nupkg. Copying one would fight the
// node's own, so neither is ever given a path.
func TestFilesTheNodeMakesAreNeverCarried(t *testing.T) {
	made := []packageFile{
		{Type: "maven", Name: "org.scene:demo", Version: "1.0", File: "maven-metadata.xml"},
		{Type: "maven", Name: "org.scene:demo", Version: "1.0", File: "maven-metadata.xml.sha1"},
		{Type: "nuget", Name: "Demo", Version: "1.0.0", File: "demo.nuspec"},
	}
	for _, f := range made {
		if !nodeBuiltFile(f.Type, f.File) {
			t.Errorf("%s %s is not recognised as the node's own", f.Type, f.File)
		}
		if _, ok := registryFetch("scene", f); ok {
			t.Errorf("%s would be fetched", f.File)
		}
		if _, _, ok := registryPublish("scene", f); ok {
			t.Errorf("%s would be published", f.File)
		}
	}
	// The files people do upload keep their paths.
	for _, f := range []packageFile{
		{Type: "maven", Name: "org.scene:demo", Version: "1.0", File: "demo-1.0.jar"},
		{Type: "nuget", Name: "Demo", Version: "1.0.0", File: "demo.1.0.0.nupkg"},
	} {
		if nodeBuiltFile(f.Type, f.File) {
			t.Errorf("%s %s was taken for the node's own", f.Type, f.File)
		}
	}
	// A maven package name that is not group:artifact is not one.
	bad := packageFile{Type: "maven", Name: "nogroup", Version: "1.0", File: "x.jar"}
	if _, ok := registryFetch("scene", bad); ok {
		t.Error("a maven package with no group was given a path")
	}
}

// Deleting one file happens where the file lives, not where uploads go:
// posting a delete to the upload endpoint would be meaningless.
func TestOnlyGenericDeletesOneFile(t *testing.T) {
	if !canDeleteFile("generic") {
		t.Error("generic cannot delete one file")
	}
	for _, typ := range []string{"maven", "nuget", "rubygems", "helm"} {
		if canDeleteFile(typ) {
			t.Errorf("%s claims it can delete one file on its own", typ)
		}
	}
}

// A package's name, version and file names are whatever whoever published
// it chose, and maven's path is laid out from them rather than escaped
// into one segment. A piece that could climb out of the path, or be read
// as an escape, gets no path at all: the package is reported instead of
// being read from or written to somewhere else on the node.
func TestPathPiecesThatCouldClimbOutAreRefused(t *testing.T) {
	bad := []packageFile{
		{Type: "generic", Name: "..", Version: "1.0", File: "demo.zip"},
		{Type: "generic", Name: "demo", Version: "..", File: "demo.zip"},
		{Type: "generic", Name: "demo", Version: "1.0", File: ".."},
		{Type: "generic", Name: "demo", Version: "1.0", File: "a/b"},
		{Type: "generic", Name: "demo", Version: "1.0", File: "%2e%2e"},
		{Type: "generic", Name: "demo", Version: "1.0", File: ""},
		{Type: "maven", Name: "org.scene:..", Version: "1.0", File: "demo.jar"},
		{Type: "maven", Name: "org...scene:demo", Version: "1.0", File: "demo.jar"},
		{Type: "maven", Name: "org.scene:demo", Version: "../../etc", File: "demo.jar"},
		{Type: "maven", Name: "org.scene:demo", Version: "1.0", File: "../secret"},
		{Type: "nuget", Name: "..", Version: "1.0.0", File: "demo.nupkg"},
		{Type: "rubygems", Name: "demo", Version: "1.0", File: "../other.gem"},
		{Type: "helm", Name: "demo", Version: "1.0", File: "..%2fother.tgz"},
	}
	for _, f := range bad {
		if path, ok := registryFetch("scene", f); ok {
			t.Errorf("%s %s %s %q was given the fetch path %q", f.Type, f.Name, f.Version, f.File, path)
		}
		if _, path, ok := registryPublish("scene", f); ok {
			t.Errorf("%s %s %s %q was given the publish path %q", f.Type, f.Name, f.Version, f.File, path)
		}
	}
	// The ordinary shapes still work: a dotted maven group, a version with
	// a suffix, a file with dots in its name.
	fine := packageFile{Type: "maven", Name: "org.scene.demo:intro-64k", Version: "2.0-SNAPSHOT", File: "intro-64k-2.0-SNAPSHOT.jar"}
	want := "/api/packages/scene/maven/org/scene/demo/intro-64k/2.0-SNAPSHOT/intro-64k-2.0-SNAPSHOT.jar"
	if got, ok := registryFetch("scene", fine); !ok || got != want {
		t.Errorf("fetch = %q (ok %v), want %q", got, ok, want)
	}
}
