package replication

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"scenegit.org/forgesync/internal/forgejo"
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
}

const testUser, testToken = "forgesync", "node-token"

func newGitNode(t *testing.T) *gitNode {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	n := &gitNode{t: t, root: t.TempDir(), lfs: map[string][]byte{}}
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
