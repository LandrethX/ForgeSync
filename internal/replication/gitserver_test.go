package replication

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"scenegit.org/forgesync/internal/forgejo"
	"strings"
	"testing"
)

// gitNode is a stand-in Forgejo node: bare repositories served by the real
// `git http-backend`, requiring the ForgeSync service account's basic auth.
type gitNode struct {
	t    *testing.T
	root string
	srv  *httptest.Server
}

const testUser, testToken = "forgesync", "node-token"

func newGitNode(t *testing.T) *gitNode {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	n := &gitNode{t: t, root: t.TempDir()}
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
