package replication

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// Packages are what a node's registries hold: a jar, a tarball, a
// container image. They belong to an owner -- a person or an organization
// -- rather than to a repository, so they're brought together once a
// round for every owner that owns a repository ForgeSync knows, the way
// organizations are.
//
// A published file doesn't change: Forgejo refuses a second upload of the
// same name in the same version, and the registry gives its sha256 back.
// So a file is a member, "<type>|<name>|<version>|<file>|<digest>", and
// they merge with internal/set like reactions or collaborators: published
// anywhere is published everywhere, and one deleted after every node had
// it is deleted everywhere. Two nodes can't disagree about what's in a
// file -- the digest is part of the member -- but they can hold different
// content under one name, and that is a conflict nobody should resolve by
// overwriting.
//
// What can travel is decided by the registry protocol, not by us. A
// package type is carried when its files can be fetched and put back by
// path alone: **generic** and **maven** can, and are. Everything else --
// container images above all, which speak the OCI protocol, and the
// package types that build an index out of what's uploaded (npm, nuget,
// pypi and the rest) -- would need its own client to publish faithfully,
// so ForgeSync doesn't pretend: a package it can't carry that isn't on
// every node is reported as such, naming the type, instead of quietly
// being left behind.
//
// Uploads are made by the service account, because the registry endpoints
// aren't the API routes that take Sudo. The copy therefore says ForgeSync
// published it, while the original keeps its author.

// The conflict kinds this pass owns.
const (
	// PackageConflictKind: a node is short of files ForgeSync could carry.
	PackageConflictKind = "package_incomplete"
	// PackageTypeConflictKind: a package of a type ForgeSync can't carry
	// isn't on every node.
	PackageTypeConflictKind = "package_unreplicated"
)

// packageMember identifies one file of one package version.
func packageMember(p forgejo.Package, f forgejo.PackageFile) string {
	digest := f.HashSHA256
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return strings.Join([]string{p.Type, p.Name, p.Version, f.Name, digest}, "|")
}

// packageFile is a member taken apart again.
type packageFile struct {
	Type, Name, Version, File, Digest string
}

func parsePackageMember(member string) (packageFile, bool) {
	parts := strings.Split(member, "|")
	if len(parts) != 5 {
		return packageFile{}, false
	}
	return packageFile{parts[0], parts[1], parts[2], parts[3], parts[4]}, true
}

func (f packageFile) version() string { return f.Type + " " + f.Name + " " + f.Version }

// What ForgeSync can carry is decided by the registry protocol, not by
// preference. A type travels when its files can be fetched by a path and
// put back as a body, and when putting one back recreates the package
// rather than half of it.
//
// Two shapes qualify. generic and maven fetch and publish at the same
// path. nuget, rubygems and helm publish to one fixed endpoint and are
// fetched from somewhere else, which is only a second path to know; and
// each of them is a **single-file** package, so putting that file back is
// the whole of it, with no index to rebuild and no partial state to leave
// behind. That is what makes them safe, not that their upload is simple.
//
// Everything else stays out. npm and composer wrap the file in JSON,
// pypi in a form with its own metadata, debian and rpm need a
// distribution and component the package API does not report, and
// container images speak a protocol of their own. Each needs its own
// client written and tested, and a package copied halfway is worse than
// one not copied at all.

// plainSegment reports whether a piece of a package's identity can stand
// in a URL path. A package's name, version and file names are whatever
// the person who published it chose, and maven's path is laid out from
// them rather than escaped into one segment, so a piece that could climb
// out of the path or be read as an escape is refused outright: ForgeSync
// fetches and publishes nothing for it, and says so, rather than reading
// or writing somewhere other than the package it meant.
func plainSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\%")
}

// nodeBuiltFile reports whether a file in a package version was made by
// the node rather than sent to it. Forgejo extracts a .nuspec from every
// .nupkg it is given, and builds maven-metadata.xml out of what it holds,
// so both appear in a node's file listing without anyone having uploaded
// one. Neither is ForgeSync's to carry: publishing the package the file
// was made from recreates it there, and publishing it on its own would
// fight the node's own copy. Left out of the member set, they also can't
// be read as a node being short of a file.
func nodeBuiltFile(typ, file string) bool {
	switch typ {
	case "maven":
		return strings.HasPrefix(file, "maven-metadata.xml")
	case "nuget":
		return strings.HasSuffix(file, ".nuspec")
	}
	return false
}

// registryFetch is where one file of a package can be read on a node. It
// is also where a single file is deleted, for the one type that allows
// it.
func registryFetch(owner string, f packageFile) (string, bool) {
	if nodeBuiltFile(f.Type, f.File) || !plainSegment(f.Name) || !plainSegment(f.Version) || !plainSegment(f.File) {
		return "", false
	}
	base := "/api/packages/" + url.PathEscape(owner)
	switch f.Type {
	case "generic":
		return base + "/generic/" + url.PathEscape(f.Name) + "/" + url.PathEscape(f.Version) +
			"/" + url.PathEscape(f.File), true
	case "maven":
		group, artifact, ok := strings.Cut(f.Name, ":")
		if !ok {
			return "", false
		}
		// This path is laid out, not escaped into one segment, so every
		// piece of it has to stand on its own.
		for _, part := range append(strings.Split(group, "."), artifact) {
			if !plainSegment(part) {
				return "", false
			}
		}
		path := strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + f.Version + "/" + f.File
		return base + "/maven/" + path, true
	case "nuget":
		return base + "/nuget/package/" + url.PathEscape(f.Name) + "/" + url.PathEscape(f.Version) +
			"/" + url.PathEscape(f.File), true
	case "rubygems":
		return base + "/rubygems/gems/" + url.PathEscape(f.File), true
	case "helm":
		return base + "/helm/" + url.PathEscape(f.File), true
	}
	return "", false
}

// registryPublish is how one file is put back. For generic and maven that
// is the path it came from; the others take the file at a fixed endpoint
// and work out for themselves what it is.
func registryPublish(owner string, f packageFile) (method, path string, ok bool) {
	if nodeBuiltFile(f.Type, f.File) || !plainSegment(f.Name) || !plainSegment(f.Version) || !plainSegment(f.File) {
		return "", "", false
	}
	base := "/api/packages/" + url.PathEscape(owner)
	switch f.Type {
	case "nuget":
		return http.MethodPut, base + "/nuget/", true
	case "rubygems":
		return http.MethodPost, base + "/rubygems/api/v1/gems/", true
	case "helm":
		return http.MethodPost, base + "/helm/api/charts", true
	}
	p, ok := registryFetch(owner, f)
	return http.MethodPut, p, ok
}

// packageTypeCarried reports whether ForgeSync can publish this type at
// all, which is what decides between the two conflicts.
func packageTypeCarried(typ string) bool {
	_, _, ok := registryPublish("x", packageFile{Type: typ, Name: "a:b", Version: "1", File: "f"})
	return ok
}

// canDeleteFile reports whether one file can be removed on its own. The
// generic registry has an endpoint for it; the others have none, so a
// single file that should go there waits for the whole version to go.
// For the single-file types that is the same thing anyway.
func canDeleteFile(typ string) bool { return typ == "generic" }

// syncPackages brings every owner's packages together. It runs once a
// round, after the repositories, like the organizations do.
func (e *Engine) syncPackages(ctx context.Context, recs []store.RepositoryRecord, healthy map[string]bool) {
	owners := map[string][]store.RepositoryRecord{}
	for _, rec := range recs {
		if rec.DeletedAt != nil {
			continue
		}
		owner, _, ok := strings.Cut(rec.FullName, "/")
		if !ok || owner == e.opts.ArchiveOrg {
			continue
		}
		owners[owner] = append(owners[owner], rec)
	}
	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		e.syncOwnerPackages(ctx, name, owners[name], healthy)
	}
}

// packagesOn is one node's packages as members, with the package each
// member came from.
func (e *Engine) packagesOn(ctx context.Context, n Node, owner string) (map[string]bool, map[string]forgejo.Package, error) {
	members := map[string]bool{}
	from := map[string]forgejo.Package{}
	for page := 1; ; page++ {
		pkgs, err := n.API.Packages(ctx, owner, page, 50)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range pkgs {
			files, err := n.API.PackageFiles(ctx, owner, p.Type, p.Name, p.Version)
			if err != nil {
				return nil, nil, err
			}
			for _, f := range files {
				if nodeBuiltFile(p.Type, f.Name) {
					continue
				}
				m := packageMember(p, f)
				members[m], from[m] = true, p
			}
		}
		if len(pkgs) < 50 {
			return members, from, nil
		}
	}
}

func (e *Engine) syncOwnerPackages(ctx context.Context, owner string, recs []store.RepositoryRecord, healthy map[string]bool) {
	at := map[string]map[string]bool{}
	unread := 0 // nodes that could not be read (see settle.go)
	for _, n := range e.order {
		if e.nodes[n].API == nil {
			continue
		}
		if !healthy[n] {
			unread++
			continue
		}
		members, _, err := e.packagesOn(ctx, e.nodes[n], owner)
		if err != nil {
			e.log.Warn("packages: reading them failed", "owner", owner, "node", n, "error", err)
			unread++
			continue
		}
		at[n] = members
	}
	if len(at) < 2 {
		return
	}
	anything := false
	for _, members := range at {
		if len(members) > 0 {
			anything = true
		}
	}
	rec, err := e.store.PackageOwner(ctx, owner)
	if err != nil {
		e.log.Error("packages: reading what was agreed failed", "owner", owner, "error", err)
		return
	}
	if !anything && rec.Base == "" {
		return // this owner has no packages anywhere
	}

	base := set.From(rec.Base)
	plan := set.Decide(at, base)
	// A published file doesn't change, so one name holding different
	// content on two nodes means one of them isn't what it says. Neither
	// travels and neither is removed: it's a conflict for a person.
	contested := contestedPackageFiles(at)
	found := contestedPackageConflicts(e.order, owner, at, contested)
	// A type ForgeSync can't publish is reported once per package version,
	// naming every node that hasn't got it, and left out of the plan.
	found = append(found, uncarriedPackages(e.order, owner, plan)...)
	for _, n := range e.order {
		if _, taking := at[n]; !taking {
			continue
		}
		found = append(found, e.removePackageFiles(ctx, owner, n, without(plan.Remove[n], contested), at)...)
		found = append(found, e.addPackageFiles(ctx, owner, n, without(plan.Add[n], contested), at)...)
	}
	if now := settle(unread, rec.Base, at, base); now != rec.Base {
		if err := e.store.SetPackageOwner(ctx, owner, now); err != nil {
			e.log.Error("packages: recording them failed", "owner", owner, "error", err)
		}
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	found = spreadOverRepos(found, recs)
	kinds := []string{PackageConflictKind, PackageTypeConflictKind}
	if _, err := e.store.SyncConflicts(ctx, found, ids, kinds, e.now()); err != nil {
		e.log.Error("packages: recording conflicts failed", "owner", owner, "error", err)
	}
}

// uncarriedPackages describes the packages ForgeSync can't publish that
// aren't on every node: one conflict per version, naming the nodes that
// haven't got it. Saying it once is the point -- a package with twenty
// files would otherwise be twenty conflicts about the same thing.
func uncarriedPackages(order []string, owner string, plan set.Plan) []store.FoundConflict {
	missing := map[string]map[string]bool{} // version -> nodes without it
	about := map[string]packageFile{}
	for node, members := range plan.Add {
		for _, m := range members {
			f, ok := parsePackageMember(m)
			if !ok || packageTypeCarried(f.Type) {
				continue
			}
			if missing[f.version()] == nil {
				missing[f.version()] = map[string]bool{}
			}
			missing[f.version()][node], about[f.version()] = true, f
		}
	}
	versions := make([]string, 0, len(missing))
	for v := range missing {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	var found []store.FoundConflict
	for _, v := range versions {
		var nodes []string
		for _, n := range order {
			if missing[v][n] {
				nodes = append(nodes, n)
			}
		}
		f := about[v]
		found = append(found, store.FoundConflict{Kind: PackageTypeConflictKind, Ref: v,
			Details: map[string]any{"owner": owner, "package_type": f.Type, "package": f.Name,
				"version": f.Version, "missing": nodes}})
	}
	return found
}

// contestedPackageFiles are the files ("<type>|<name>|<version>|<file>")
// published with different content on different nodes.
func contestedPackageFiles(at map[string]map[string]bool) map[string]bool {
	digests := map[string]map[string]bool{}
	for _, members := range at {
		for m := range members {
			f, ok := parsePackageMember(m)
			if !ok {
				continue
			}
			name := f.version() + "|" + f.File
			if digests[name] == nil {
				digests[name] = map[string]bool{}
			}
			digests[name][f.Digest] = true
		}
	}
	out := map[string]bool{}
	for name, ds := range digests {
		if len(ds) > 1 {
			out[name] = true
		}
	}
	return out
}

// without drops the members of contested files from a plan.
func without(members []string, contested map[string]bool) []string {
	var out []string
	for _, m := range members {
		f, ok := parsePackageMember(m)
		if ok && contested[f.version()+"|"+f.File] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// contestedPackageConflicts says what each node holds under a contested
// name, so a person can see which copy is which.
func contestedPackageConflicts(order []string, owner string, at map[string]map[string]bool, contested map[string]bool) []store.FoundConflict {
	names := make([]string, 0, len(contested))
	for name := range contested {
		names = append(names, name)
	}
	sort.Strings(names)
	var found []store.FoundConflict
	for _, name := range names {
		digests := map[string]any{}
		var one packageFile
		for _, n := range order {
			for m := range at[n] {
				f, ok := parsePackageMember(m)
				if ok && f.version()+"|"+f.File == name {
					digests[n], one = f.Digest, f
				}
			}
		}
		found = append(found, store.FoundConflict{Kind: PackageConflictKind, Ref: one.version() + " " + one.File,
			Details: map[string]any{"owner": owner, "package_type": one.Type, "package": one.Name,
				"version": one.Version, "file": one.File, "digests": digests,
				"reason": "different content is published under this name on different nodes"}})
	}
	return found
}

// spreadOverRepos records each conflict against every repository of the
// owner: a package has no repository of its own, and a repository is
// where a person looks. Conflicts are built without an id, so this is
// what gives them one.
func spreadOverRepos(found []store.FoundConflict, recs []store.RepositoryRecord) []store.FoundConflict {
	out := make([]store.FoundConflict, 0, len(found)*len(recs))
	for _, c := range found {
		for _, r := range recs {
			with := c
			with.RepositoryID = r.ID
			out = append(out, with)
		}
	}
	return out
}

// addPackageFiles publishes on node n the files the other nodes have.
func (e *Engine) addPackageFiles(ctx context.Context, owner, n string, add []string, at map[string]map[string]bool) []store.FoundConflict {
	var found []store.FoundConflict
	for _, m := range add {
		f, ok := parsePackageMember(m)
		if !ok || !packageTypeCarried(f.Type) {
			continue // reported once, in uncarriedPackages
		}
		from, ok := packageSource(e.order, at, m, n)
		if !ok {
			continue
		}
		if err := e.carryPackageFile(ctx, owner, f, e.nodes[from], e.nodes[n]); err != nil {
			e.log.Warn("packages: copying a file failed; left for the next run", "owner", owner,
				"package", f.version(), "file", f.File, "from", from, "to", n, "error", err)
			found = append(found, store.FoundConflict{Kind: PackageConflictKind, Ref: f.version() + " " + f.File,
				Details: map[string]any{"owner": owner, "package_type": f.Type, "package": f.Name, "version": f.Version,
					"file": f.File, "node": n, "reason": err.Error()}})
			continue
		}
		at[n][m] = true
		e.log.Info("package file published", "owner", owner, "package", f.version(), "file", f.File,
			"from", from, "to", n)
		e.audit(ctx, "repo.package_published", owner+"/"+f.Name, map[string]any{"owner": owner,
			"package_type": f.Type, "package": f.Name, "version": f.Version, "file": f.File, "from": from, "node": n})
	}
	return found
}

// removePackageFiles takes away on node n what every node has let go.
// A version whose files are all going is deleted whole, which is the only
// way for a type whose registry has no endpoint for one file.
func (e *Engine) removePackageFiles(ctx context.Context, owner, n string, remove []string, at map[string]map[string]bool) []store.FoundConflict {
	var found []store.FoundConflict
	whole := map[string]bool{} // versions of which nothing is left
	going := map[string][]packageFile{}
	for _, m := range remove {
		f, ok := parsePackageMember(m)
		if !ok {
			continue
		}
		going[f.version()] = append(going[f.version()], f)
	}
	for version, files := range going {
		if len(files) == packageFilesOn(at[n], files[0]) {
			whole[version] = true
		}
	}
	for _, m := range remove {
		f, ok := parsePackageMember(m)
		if !ok {
			continue
		}
		var err error
		switch {
		case whole[f.version()]:
			err = e.nodes[n].API.DeletePackage(ctx, owner, f.Type, f.Name, f.Version)
			if forgejo.IsNotFound(err) {
				err = nil // already gone
			}
		case canDeleteFile(f.Type):
			err = e.packageRequest(ctx, e.nodes[n], http.MethodDelete, owner, f, nil, 0)
		default:
			// Neither the whole version nor this one file can go: leave it
			// and say so rather than deleting more than was asked.
			found = append(found, store.FoundConflict{Kind: PackageConflictKind, Ref: f.version() + " " + f.File,
				Details: map[string]any{"owner": owner, "package_type": f.Type, "package": f.Name, "version": f.Version,
					"file": f.File, "node": n,
					"reason": "it was deleted elsewhere, but " + f.Type + " packages can only be deleted whole"}})
			continue
		}
		if err != nil {
			e.log.Warn("packages: removing a file failed; left for the next run", "owner", owner,
				"package", f.version(), "file", f.File, "node", n, "error", err)
			continue
		}
		delete(at[n], m)
		if whole[f.version()] {
			// One call took the whole version, so the rest are gone too.
			for other := range at[n] {
				if o, ok := parsePackageMember(other); ok && o.version() == f.version() {
					delete(at[n], other)
				}
			}
		}
		e.log.Info("package file removed", "owner", owner, "package", f.version(), "file", f.File, "node", n)
		e.audit(ctx, "repo.package_removed", owner+"/"+f.Name, map[string]any{"owner": owner,
			"package_type": f.Type, "package": f.Name, "version": f.Version, "file": f.File, "node": n})
	}
	return found
}

// packageFilesOn counts what the node has of one package version.
func packageFilesOn(members map[string]bool, of packageFile) int {
	n := 0
	for m := range members {
		if f, ok := parsePackageMember(m); ok && f.version() == of.version() {
			n++
		}
	}
	return n
}

// packageSource picks a node that has the file, in the configured order.
func packageSource(order []string, at map[string]map[string]bool, member, not string) (string, bool) {
	for _, n := range order {
		if n != not && at[n][member] {
			return n, true
		}
	}
	return "", false
}

// carryPackageFile fetches one file and publishes it on the other node,
// through a file on disk so a large one costs disk rather than memory.
func (e *Engine) carryPackageFile(ctx context.Context, owner string, f packageFile, from, to Node) error {
	tmp, err := os.CreateTemp(e.git.WorkDir, "package-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	size, err := e.fetchPackageFile(ctx, from, owner, f, tmp)
	if err != nil {
		return fmt.Errorf("from %s: %w", from.Name, err)
	}
	if e.opts.PackageMax > 0 && size > e.opts.PackageMax {
		return fmt.Errorf("the file is %d bytes, over replication.package_max_bytes (%d)", size, e.opts.PackageMax)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := e.packageRequest(ctx, to, http.MethodPut, owner, f, tmp, size); err != nil {
		return fmt.Errorf("to %s: %w", to.Name, err)
	}
	return nil
}

func (e *Engine) fetchPackageFile(ctx context.Context, n Node, owner string, f packageFile, w io.Writer) (int64, error) {
	path, ok := registryFetch(owner, f)
	if !ok {
		return 0, fmt.Errorf("ForgeSync can't say where %q would be in the %s registry", f.File, f.Type)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(n.URL, "/")+path, nil)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth(n.User, n.Token)
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, registryStatus(resp)
	}
	return io.Copy(w, resp.Body)
}

// packageRequest publishes or removes one file through the registry.
func (e *Engine) packageRequest(ctx context.Context, n Node, method, owner string, f packageFile, body io.Reader, size int64) error {
	var path string
	var ok bool
	if method == http.MethodDelete {
		// Deleting one file happens where it lives, not where uploads go.
		path, ok = registryFetch(owner, f)
	} else {
		method, path, ok = registryPublish(owner, f)
	}
	if !ok {
		return fmt.Errorf("ForgeSync can't say where %q would go in the %s registry", f.File, f.Type)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.URL, "/")+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(n.User, n.Token)
	if body != nil {
		// Forgejo reads the body as a form unless it's told otherwise, and
		// then refuses the upload.
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = size
	}
	resp, err := e.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return registryStatus(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// registryStatus turns a refusal into an error that says what the node said.
func registryStatus(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		msg = resp.Status
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("%s: %s", resp.Status, msg)
}
