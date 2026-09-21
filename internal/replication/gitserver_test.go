package replication

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"scenegit.org/forgesync/internal/forgejo"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// gitNode is a stand-in Forgejo node: bare repositories served by the real
// `git http-backend`, requiring the ForgeSync service account's basic auth.
type gitNode struct {
	t    *testing.T
	root string
	srv  *httptest.Server

	// lfs is this node's LFS store: oid -> the object's bytes. lfsOff
	// makes the node answer as one with LFS turned off.
	lfsMu  sync.Mutex
	lfs    map[string][]byte
	lfsOff bool

	// pkgs is this node's package registry: a version's files by name.
	// The REST API (fakeAPI) and the registry endpoints below are the same
	// node, as they are in Forgejo.
	pkgMu sync.Mutex
	pkgs  map[pkgKey]map[string][]byte
}

// pkgKey identifies one package version in a node's registry.
type pkgKey struct{ owner, typ, name, version string }

const testUser, testToken = "forgesync", "node-token"

func newGitNode(t *testing.T) *gitNode {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	n := &gitNode{t: t, root: t.TempDir(), lfs: map[string][]byte{}, pkgs: map[pkgKey]map[string][]byte{}}
	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + n.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + n.root},
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testToken))
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="forgejo"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/packages/") {
			n.serveRegistry(w, r)
			return
		}
		if i := strings.Index(r.URL.Path, "/info/lfs/"); i >= 0 {
			n.serveLFS(w, r, r.URL.Path[i+len("/info/lfs"):])
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if i := strings.Index(name, ".git/"); i >= 0 {
			name = name[:i+4]
		}
		if _, err := os.Stat(filepath.Join(n.root, name)); err != nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// create makes an empty bare repository that accepts pushes over HTTP.
func (n *gitNode) create(name string) string {
	n.t.Helper()
	dir := filepath.Join(n.root, name+".git")
	n.git("", "init", "--quiet", "--bare", dir)
	n.git(dir, "config", "http.receivepack", "true")
	return dir
}

// createWithCommit makes a repository that already has one commit on
// main, as Forgejo's first wiki page does.
func (n *gitNode) createWithCommit(name, msg string) string {
	n.t.Helper()
	dir := n.create(name)
	work := n.t.TempDir()
	runGit(n.t, "", "init", "--quiet", "--initial-branch=main", work)
	os.WriteFile(filepath.Join(work, "Home.md"), []byte(msg+"\n"), 0o644)
	runGit(n.t, work, "add", "Home.md")
	runGit(n.t, work, "commit", "--quiet", "-m", msg)
	runGit(n.t, work, "push", "--quiet", dir, "main:main")
	return dir
}

func (n *gitNode) remote(name string) Remote {
	return Remote{URL: n.srv.URL + "/" + name + ".git", User: testUser, Token: testToken}
}

// refs returns the repository's branches and tags as the server has them.
func (n *gitNode) refs(name string) Refs {
	n.t.Helper()
	out := n.git(filepath.Join(n.root, name+".git"), "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	refs := Refs{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if sha, ref, ok := strings.Cut(line, " "); ok {
			refs[ref] = sha
		}
	}
	return refs
}

func (n *gitNode) git(dir string, args ...string) string {
	n.t.Helper()
	return runGit(n.t, dir, args...)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// workTree is a local clone used to make commits and push them directly
// to a node, like a user would.
type workTree struct {
	t   *testing.T
	dir string
}

func newWorkTree(t *testing.T) *workTree {
	w := &workTree{t: t, dir: t.TempDir()}
	runGit(t, w.dir, "init", "--quiet", "-b", "main")
	return w
}

// commit adds a commit on the current branch and returns its id.
func (w *workTree) commit(msg string) string {
	w.t.Helper()
	os.WriteFile(filepath.Join(w.dir, "f"), []byte(msg), 0o644)
	runGit(w.t, w.dir, "add", "f")
	runGit(w.t, w.dir, "commit", "--quiet", "-m", msg)
	return strings.TrimSpace(runGit(w.t, w.dir, "rev-parse", "HEAD"))
}

// push force-pushes refspecs straight into a node's repository directory,
// as a user with write access (or a history rewrite) would.
func (w *workTree) push(n *gitNode, name string, refspecs ...string) {
	w.t.Helper()
	runGit(w.t, w.dir, append([]string{"push", "--quiet", "--force", filepath.Join(n.root, name+".git")}, refspecs...)...)
}

func testGit(t *testing.T) *Git {
	return &Git{WorkDir: t.TempDir()}
}

func bg() context.Context { return context.Background() }

// apiRepo is alice/demo as the fake API describes it.
func (n *gitNode) apiRepo() forgejo.Repository {
	return forgejo.Repository{FullName: "alice/demo", Name: "demo", Owner: forgejo.User{Login: "alice"}, DefaultBranch: "main"}
}

// serveLFS answers the Git LFS batch API the way a Forgejo node does: a
// batch request says what to do with each object, and the hrefs it gives
// back are this same server. Only what ForgeSync uses is implemented.
func (n *gitNode) serveLFS(w http.ResponseWriter, r *http.Request, path string) {
	if n.lfsOff {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	base := strings.TrimSuffix(n.srv.URL+r.URL.Path[:strings.Index(r.URL.Path, "/info/lfs/")+len("/info/lfs")], "/")
	switch {
	case path == "/objects/batch" && r.Method == http.MethodPost:
		var req struct {
			Operation string `json:"operation"`
			Objects   []struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"objects"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type action struct {
			Href string `json:"href"`
		}
		type object struct {
			OID     string            `json:"oid"`
			Size    int64             `json:"size"`
			Actions map[string]action `json:"actions,omitempty"`
			Error   *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		var out struct {
			Transfer string   `json:"transfer"`
			Objects  []object `json:"objects"`
		}
		out.Transfer = "basic"
		for _, o := range req.Objects {
			n.lfsMu.Lock()
			_, here := n.lfs[o.OID]
			n.lfsMu.Unlock()
			obj := object{OID: o.OID, Size: o.Size}
			switch {
			case req.Operation == "upload" && !here:
				// Only what's missing comes back with something to do.
				obj.Actions = map[string]action{"upload": {Href: base + "/objects/" + o.OID}}
			case req.Operation == "download" && here:
				obj.Actions = map[string]action{"download": {Href: base + "/objects/" + o.OID}}
			case req.Operation == "download":
				obj.Error = &struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}{Code: 404, Message: "object does not exist"}
			}
			out.Objects = append(out.Objects, obj)
		}
		w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(path, "/objects/") && r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n.lfsMu.Lock()
		n.lfs[strings.TrimPrefix(path, "/objects/")] = body
		n.lfsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(path, "/objects/") && r.Method == http.MethodGet:
		n.lfsMu.Lock()
		body, ok := n.lfs[strings.TrimPrefix(path, "/objects/")]
		n.lfsMu.Unlock()
		if !ok {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		w.Write(body)
	default:
		http.Error(w, "Not found", http.StatusNotFound)
	}
}

// putLFS puts an object in this node's LFS store, as a push with git-lfs
// would, and returns its oid.
func (n *gitNode) putLFS(content string) string {
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	n.lfsMu.Lock()
	defer n.lfsMu.Unlock()
	n.lfs[oid] = []byte(content)
	return oid
}

// hasLFS reports whether the node's LFS store holds that content.
func (n *gitNode) hasLFS(content string) bool {
	sum := sha256.Sum256([]byte(content))
	n.lfsMu.Lock()
	defer n.lfsMu.Unlock()
	body, ok := n.lfs[hex.EncodeToString(sum[:])]
	return ok && string(body) == content
}

// pointerFor is the pointer file git carries for that content.
func pointerFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "version https://git-lfs.github.com/spec/v1\noid sha256:" + hex.EncodeToString(sum[:]) +
		"\nsize " + strconv.Itoa(len(content)) + "\n"
}

// serveRegistry answers the package registry endpoints ForgeSync uses.
// generic and maven fetch and publish at the same path: generic keeps a
// file under its package name and version, maven lays a package out as a
// Maven repository is. nuget, rubygems and helm are read by path but
// uploaded to one endpoint that names nothing, so the node works out what
// the package is by reading the file, which is what acceptUpload does.
// Publishing the same name twice is refused, as Forgejo refuses it.
func (n *gitNode) serveRegistry(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/packages/")
	if owner, rest, cut := strings.Cut(path, "/"); cut {
		if typ, ok := uploadEndpoint(rest); ok {
			n.acceptUpload(w, r, owner, typ)
			return
		}
	}
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	key, file, ok := n.parseRegistryPath(path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if _, taken := n.pkgs[key][file]; taken {
			http.Error(w, "file already exists", http.StatusConflict)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
			// Forgejo reads the body as a form otherwise and refuses it.
			http.Error(w, "request Content-Type isn't multipart/form-data", http.StatusInternalServerError)
			return
		}
		if n.pkgs[key] == nil {
			n.pkgs[key] = map[string][]byte{}
		}
		n.pkgs[key][file] = body
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		body, here := n.pkgs[key][file]
		if !here {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Write(body)
	case http.MethodDelete:
		if _, here := n.pkgs[key][file]; !here {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		delete(n.pkgs[key], file)
		if len(n.pkgs[key]) == 0 {
			delete(n.pkgs, key)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not allowed", http.StatusMethodNotAllowed)
	}
}

// parseRegistryPath reads "<owner>/<type>/..." the way each registry lays
// its files out.
func (n *gitNode) parseRegistryPath(path string) (pkgKey, string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) < 3 {
		return pkgKey{}, "", false
	}
	owner, typ, rest := parts[0], parts[1], parts[2:]
	switch typ {
	case "generic":
		if len(rest) != 3 {
			return pkgKey{}, "", false
		}
		return pkgKey{owner, typ, rest[0], rest[1]}, rest[2], true
	case "maven":
		if len(rest) < 4 {
			return pkgKey{}, "", false
		}
		file := rest[len(rest)-1]
		version := rest[len(rest)-2]
		artifact := rest[len(rest)-3]
		group := strings.Join(rest[:len(rest)-3], ".")
		return pkgKey{owner, typ, group + ":" + artifact, version}, file, true
	case "nuget":
		// /nuget/package/{id}/{version}/{file}
		if len(rest) != 4 || rest[0] != "package" {
			return pkgKey{}, "", false
		}
		return pkgKey{owner, typ, rest[1], rest[2]}, rest[3], true
	case "rubygems":
		// /rubygems/gems/{file}, which names neither package nor version.
		if len(rest) != 2 || rest[0] != "gems" {
			return pkgKey{}, "", false
		}
		key, ok := n.findPackageByFile(owner, typ, rest[1])
		return key, rest[1], ok
	case "helm":
		// /helm/{file}, likewise.
		if len(rest) != 1 {
			return pkgKey{}, "", false
		}
		key, ok := n.findPackageByFile(owner, typ, rest[0])
		return key, rest[0], ok
	}
	return pkgKey{}, "", false
}

// findPackageByFile works back from a file name to the package it belongs
// to, which is what the rubygems and helm registries do: their download
// paths carry the file and nothing else, so the node has to look it up.
// The caller holds pkgMu.
func (n *gitNode) findPackageByFile(owner, typ, file string) (pkgKey, bool) {
	for key, files := range n.pkgs {
		if key.owner != owner || key.typ != typ {
			continue
		}
		if _, here := files[file]; here {
			return key, true
		}
	}
	return pkgKey{}, false
}

// uploadEndpoint names the type whose upload lands at this path. These
// endpoints say nothing about the package: the node reads the file.
func uploadEndpoint(rest string) (string, bool) {
	switch strings.TrimSuffix(rest, "/") {
	case "nuget":
		return "nuget", true
	case "rubygems/api/v1/gems":
		return "rubygems", true
	case "helm/api/charts":
		return "helm", true
	}
	return "", false
}

// acceptUpload takes a package at the endpoint its registry uploads to,
// reads what it is out of the file itself, and stores it under the name
// that registry gives it. nuget also keeps the .nuspec it finds inside
// the .nupkg, as Forgejo does: a file nobody uploaded, which is exactly
// what ForgeSync has to leave alone.
func (n *gitNode) acceptUpload(w http.ResponseWriter, r *http.Request, owner, typ string) {
	if r.Method == http.MethodGet {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
		// Forgejo reads the body as a form otherwise and refuses it.
		http.Error(w, "request Content-Type isn't multipart/form-data", http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name, version, file, derived, err := describeUpload(typ, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := pkgKey{owner, typ, name, version}
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	if _, taken := n.pkgs[key][file]; taken {
		http.Error(w, "file already exists", http.StatusConflict)
		return
	}
	if n.pkgs[key] == nil {
		n.pkgs[key] = map[string][]byte{}
	}
	n.pkgs[key][file] = body
	for dname, dbody := range derived {
		n.pkgs[key][dname] = dbody
	}
	w.WriteHeader(http.StatusCreated)
}

// describeUpload reads a package's identity out of the file, as each
// registry does: nuget from the .nuspec inside the .nupkg zip, rubygems
// from the metadata.gz inside the gem tar, helm from the Chart.yaml
// inside the chart. All three lowercase the file name they store.
func describeUpload(typ string, body []byte) (name, version, file string, derived map[string][]byte, err error) {
	switch typ {
	case "nuget":
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return "", "", "", nil, fmt.Errorf("not a nupkg: %w", err)
		}
		for _, f := range zr.File {
			if !strings.HasSuffix(f.Name, ".nuspec") {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return "", "", "", nil, err
			}
			spec, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return "", "", "", nil, err
			}
			id := between(string(spec), "<id>", "</id>")
			ver := between(string(spec), "<version>", "</version>")
			if id == "" || ver == "" {
				return "", "", "", nil, fmt.Errorf("the nuspec names no id or version")
			}
			low := strings.ToLower(id)
			return id, ver, low + "." + ver + ".nupkg",
				map[string][]byte{low + ".nuspec": spec}, nil
		}
		return "", "", "", nil, fmt.Errorf("the nupkg holds no nuspec")
	case "rubygems":
		meta, err := fileInTar(body, "metadata.gz")
		if err != nil {
			return "", "", "", nil, err
		}
		meta, err = ungzip(meta)
		if err != nil {
			return "", "", "", nil, err
		}
		name, version = gemNameVersion(string(meta))
		if name == "" || version == "" {
			return "", "", "", nil, fmt.Errorf("the gem's metadata names no name or version")
		}
		return name, version, strings.ToLower(name+"-"+version) + ".gem", nil, nil
	case "helm":
		plain, err := ungzip(body)
		if err != nil {
			return "", "", "", nil, fmt.Errorf("not a chart: %w", err)
		}
		chart, err := fileInTar(plain, "Chart.yaml")
		if err != nil {
			return "", "", "", nil, err
		}
		name, version = yamlField(string(chart), "name"), yamlField(string(chart), "version")
		if name == "" || version == "" {
			return "", "", "", nil, fmt.Errorf("the Chart.yaml names no name or version")
		}
		return name, version, strings.ToLower(name+"-"+version) + ".tgz", nil, nil
	}
	return "", "", "", nil, fmt.Errorf("no upload endpoint for %s", typ)
}

func between(s, open, close string) string {
	_, rest, ok := strings.Cut(s, open)
	if !ok {
		return ""
	}
	out, _, ok := strings.Cut(rest, close)
	if !ok {
		return ""
	}
	return out
}

// yamlField reads a top-level "key: value" line, which is as much YAML as
// a Chart.yaml needs here.
func yamlField(doc, key string) string {
	for _, line := range strings.Split(doc, "\n") {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// gemNameVersion reads a gemspec, where the version is nested one level
// under its own key:
//
//	name: demo
//	version: !ruby/object:Gem::Version
//	  version: 1.0.0
func gemNameVersion(doc string) (string, string) {
	name := yamlField(doc, "name")
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "version:") || i+1 >= len(lines) {
			continue
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(lines[i+1]), "version:"); ok {
			return name, strings.TrimSpace(v)
		}
	}
	return name, ""
}

// fileInTar returns the one entry whose name ends in want.
func fileInTar(archive []byte, want string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the archive holds no %s", want)
		}
		if err != nil {
			return nil, err
		}
		if h.Name == want || strings.HasSuffix(h.Name, "/"+want) {
			return io.ReadAll(tr)
		}
	}
}

func ungzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// nupkgFor, gemFor and chartFor build the smallest file each registry
// will accept, so a test can upload one the way a client would.
func nupkgFor(id, version string) []byte {
	spec := "<?xml version=\"1.0\"?><package><metadata><id>" + id +
		"</id><version>" + version + "</version></metadata></package>"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(id + ".nuspec")
	if err != nil {
		panic(err)
	}
	if _, err := f.Write([]byte(spec)); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func gemFor(name, version string) []byte {
	meta := "--- !ruby/object:Gem::Specification\nname: " + name +
		"\nversion: !ruby/object:Gem::Version\n  version: " + version + "\nplatform: ruby\n"
	return tarOf(map[string][]byte{"metadata.gz": gzipOf([]byte(meta))})
}

func chartFor(name, version string) []byte {
	chart := "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n"
	return gzipOf(tarOf(map[string][]byte{name + "/Chart.yaml": []byte(chart)}))
}

func tarOf(files map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
			panic(err)
		}
		if _, err := tw.Write(files[name]); err != nil {
			panic(err)
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func gzipOf(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// publish puts a file in the registry directly, as a client's upload does.
func (n *gitNode) publish(owner, typ, name, version, file, content string) {
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	key := pkgKey{owner, typ, name, version}
	if n.pkgs[key] == nil {
		n.pkgs[key] = map[string][]byte{}
	}
	n.pkgs[key][file] = []byte(content)
}

// upload puts a package on the node the way a client does: the node reads
// the file and decides what it is called, so a test gets whatever Forgejo
// would have stored, derived files and all.
func (n *gitNode) upload(owner, typ string, body []byte) {
	name, version, file, derived, err := describeUpload(typ, body)
	if err != nil {
		n.t.Fatalf("upload %s: %v", typ, err)
	}
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	key := pkgKey{owner, typ, name, version}
	if n.pkgs[key] == nil {
		n.pkgs[key] = map[string][]byte{}
	}
	n.pkgs[key][file] = body
	for dname, dbody := range derived {
		n.pkgs[key][dname] = dbody
	}
}

// fileNames is what one package version holds on this node, sorted.
func (n *gitNode) fileNames(owner, typ, name, version string) []string {
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	var out []string
	for file := range n.pkgs[pkgKey{owner, typ, name, version}] {
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

// fileBody is one file's content, or nil where the node hasn't got it.
func (n *gitNode) fileBody(owner, typ, name, version, file string) []byte {
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	return n.pkgs[pkgKey{owner, typ, name, version}][file]
}

// dropPackage takes a whole version away, as someone deleting it does.
func (n *gitNode) dropPackage(owner, typ, name, version string) {
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	delete(n.pkgs, pkgKey{owner, typ, name, version})
}

// packageList is what the node holds, as "<type> <name> <version> <file>=<content>",
// sorted, for a test to compare.
func (n *gitNode) packageList() string {
	n.pkgMu.Lock()
	defer n.pkgMu.Unlock()
	var out []string
	for key, files := range n.pkgs {
		for file, body := range files {
			out = append(out, key.typ+" "+key.name+" "+key.version+" "+file+"="+string(body))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
