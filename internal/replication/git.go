package replication

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Remote is a repository on a Forgejo node, reached over HTTP(S) with the
// ForgeSync service account's token.
type Remote struct {
	URL   string // e.g. https://forgejo-se.example/alice/demo.git
	User  string
	Token string
}

// ErrRepoNotFound means the repository doesn't exist on that node.
var ErrRepoNotFound = errors.New("repository not found")

// Git runs the git command line. Each repository gets a bare cache under
// WorkDir that objects are fetched into and pushed from.
type Git struct {
	Bin     string // default "git"
	WorkDir string
	Timeout time.Duration // per git command; default 10 minutes
}

// Version creates the work directory if needed (readable only by this
// user) and checks that git is usable and new enough (2.32, for
// GIT_CONFIG_GLOBAL and config from the environment).
func (g *Git) Version(ctx context.Context) (string, error) {
	if g.WorkDir != "" {
		if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
			return "", fmt.Errorf("creating the work directory: %w", err)
		}
	}
	out, _, err := g.run(ctx, "", nil, "version")
	if err != nil {
		return "", fmt.Errorf("git isn't usable: %w", err)
	}
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "git version "))
	var major, minor int
	fmt.Sscanf(v, "%d.%d", &major, &minor)
	if major < 2 || (major == 2 && minor < 32) {
		return v, fmt.Errorf("git %s is too old; replication needs 2.32 or later", v)
	}
	return v, nil
}

var safeName = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// Cache returns the bare cache repository for a repository id, creating it.
func (g *Git) Cache(ctx context.Context, id string) (string, error) {
	if !safeName.MatchString(id) {
		return "", fmt.Errorf("bad cache name %q", id)
	}
	dir := filepath.Join(g.WorkDir, id+".git")
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(g.WorkDir, 0o700); err != nil {
		return "", err
	}
	if _, _, err := g.run(ctx, "", nil, "init", "--quiet", "--bare", dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Forget removes the caches belonging to a repository id: its own and
// its wiki's. A cache is only a cache, but nothing else ever removed one,
// so every repository ForgeSync replicated and then forgot left a full
// bare mirror on the controller's disk for good. On an installation where
// repositories come and go that grows without bound, and the git cache is
// already the largest thing a controller keeps.
//
// It is deliberately forgiving: a cache that is not there is not an
// error, because forgetting must not be blocked by tidying.
func (g *Git) Forget(id string) error {
	if !safeName.MatchString(id) || g.WorkDir == "" {
		return fmt.Errorf("bad cache name %q", id)
	}
	var first error
	for _, name := range []string{id + ".git", id + "-wiki.git"} {
		if err := os.RemoveAll(filepath.Join(g.WorkDir, name)); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// LsRemote lists the branches and tags of a remote repository.
func (g *Git) LsRemote(ctx context.Context, r Remote) (Refs, error) {
	out, stderr, err := g.run(ctx, "", &r, "ls-remote", "--heads", "--tags", r.URL)
	if err != nil {
		if notFound(stderr) {
			return nil, ErrRepoNotFound
		}
		return nil, err
	}
	refs := Refs{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		sha, ref, ok := strings.Cut(sc.Text(), "\t")
		if !ok || strings.HasSuffix(ref, "^{}") || !Replicated(ref) {
			continue
		}
		refs[ref] = sha
	}
	return refs, sc.Err()
}

// Fetch copies a remote's branches and tags into the cache under
// refs/forgesync/<prefix>/, so their objects are available locally.
func (g *Git) Fetch(ctx context.Context, dir, prefix string, r Remote) error {
	_, stderr, err := g.run(ctx, dir, &r, "fetch", "--quiet", "--no-tags", "--prune", "--no-write-fetch-head", r.URL,
		"+refs/heads/*:refs/forgesync/"+prefix+"/heads/*",
		"+refs/tags/*:refs/forgesync/"+prefix+"/tags/*")
	if err != nil && notFound(stderr) {
		return ErrRepoNotFound
	}
	return err
}

// IsAncestor reports whether commit a is an ancestor of b (or equal) in the
// cache. ok is false if git couldn't tell (e.g. a commit is missing).
func (g *Git) IsAncestor(ctx context.Context, dir, a, b string) (is, ok bool) {
	_, _, err := g.run(ctx, dir, nil, "merge-base", "--is-ancestor", a, b)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, true
	}
	return false, false
}

// MergeBase returns the best common ancestor of a and b, or "" if they
// share no history.
func (g *Git) MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, _, err := g.run(ctx, dir, nil, "merge-base", a, b)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(out) == "" {
		return "", nil
	}
	return strings.TrimSpace(out), err
}

// PushResult is the outcome of one ref update.
type PushResult struct {
	OK     bool
	Reason string // why it was rejected
}

// Push applies the actions to a remote. Each ref is conditional on the
// remote still having Expected (--force-with-lease), and each succeeds or
// fails on its own.
func (g *Git) Push(ctx context.Context, dir string, r Remote, actions []Action) (map[string]PushResult, error) {
	if len(actions) == 0 {
		return map[string]PushResult{}, nil
	}
	args := []string{"push", "--porcelain", "--no-verify"}
	var specs []string
	for _, a := range actions {
		// Expected "" means the ref must not exist yet.
		args = append(args, "--force-with-lease="+a.Ref+":"+a.Expected)
		if a.Kind == Delete {
			specs = append(specs, ":"+a.Ref)
		} else {
			specs = append(specs, a.Target+":"+a.Ref)
		}
	}
	args = append(append(args, r.URL), specs...)

	out, stderr, err := g.run(ctx, dir, &r, args...)
	results := parsePorcelain(out)
	if len(results) == 0 {
		if err == nil {
			err = errors.New("git push reported nothing")
		}
		if notFound(stderr) {
			return nil, ErrRepoNotFound
		}
		return nil, err
	}
	// A rejected ref makes git exit non-zero; the per-ref results say which.
	for _, a := range actions {
		if _, ok := results[a.Ref]; !ok {
			results[a.Ref] = PushResult{Reason: "no result from git push"}
		}
	}
	return results, nil
}

// parsePorcelain reads `git push --porcelain` lines:
// "<flag>\t<from>:<to>\t<summary>", flag "!" meaning rejected.
func parsePorcelain(out string) map[string]PushResult {
	results := map[string]PushResult{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 3 || line[1] != '\t' {
			continue
		}
		parts := strings.SplitN(line[2:], "\t", 2)
		_, to, ok := strings.Cut(parts[0], ":")
		if !ok {
			continue
		}
		res := PushResult{OK: line[0] != '!'}
		if !res.OK && len(parts) == 2 {
			res.Reason = parts[1]
		}
		results[to] = res
	}
	return results
}

func notFound(stderr string) bool {
	s := strings.ToLower(stderr)
	if strings.Contains(s, "not found") || strings.Contains(s, "404") || strings.Contains(s, "does not exist") {
		return true
	}
	// A redirect means the name moved: not here under this name.
	for _, code := range []string{"301", "302", "307", "308"} {
		if strings.Contains(s, "returned error: "+code) {
			return true
		}
	}
	return false
}

// run executes git with a clean environment: no user or system config, no
// prompts. Credentials go in an HTTP header set through the environment, so
// they never appear in the process list.
func (g *Git) run(ctx context.Context, dir string, auth *Remote, args ...string) (string, string, error) {
	return g.runWith(ctx, dir, "", auth, args...)
}

// runStdin runs git with input on its standard input, for the batch
// commands that take a list of objects.
func (g *Git) runStdin(ctx context.Context, dir, stdin string, args ...string) (string, string, error) {
	return g.runWith(ctx, dir, stdin, nil, args...)
}

func (g *Git) runWith(ctx context.Context, dir, stdin string, auth *Remote, args ...string) (string, string, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	} else if g.WorkDir != "" {
		cmd.Dir = g.WorkDir
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.WorkDir,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	// Never follow redirects: ForgeSync addresses repositories by their exact
	// name, and Forgejo redirects an old name after a rename or transfer
	// (e.g. to the archived copy of a deleted repository).
	cfg := [][2]string{{"http.followRedirects", "false"}}
	if auth != nil && auth.Token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(auth.User + ":" + auth.Token))
		cfg = append(cfg, [2]string{"http.extraHeader", "Authorization: Basic " + basic})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		err = fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.String(), stderr.String(), err
}

// Pointer is one Git LFS object referenced by a pointer file in the
// repository: the file in git holds the oid and the size, and the bytes
// themselves live in the node's LFS store.
type Pointer struct {
	OID  string // sha256, as the pointer file spells it
	Size int64
}

// pointerMax is the largest blob that can be a pointer file. A pointer is
// three short lines; the spec allows a few extra ones, never this many.
const pointerMax = 1024

// Pointers returns the LFS objects referenced anywhere under the given ref
// patterns in the cache (git glob patterns, e.g. refs/heads/*).
//
// Every candidate is read, not guessed: rev-list lists the objects those
// refs reach, keeping only blobs small enough to be a pointer file, and
// each one is then read and parsed. A file that merely looks small isn't
// counted, and a real pointer is never missed because it sits in an old
// commit or on a branch nobody has checked out.
func (g *Git) Pointers(ctx context.Context, dir string, patterns []string) ([]Pointer, error) {
	args := []string{"rev-list", "--objects", "--no-object-names",
		fmt.Sprintf("--filter=blob:limit=%d", pointerMax)}
	for _, p := range patterns {
		args = append(args, "--glob="+p)
	}
	out, _, err := g.run(ctx, dir, nil, args...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	// rev-list lists commits and trees too; ask which of them are blobs.
	check, _, err := g.runStdin(ctx, dir, strings.Join(ids, "\n")+"\n",
		"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return nil, err
	}
	var blobs []string
	for _, line := range strings.Split(check, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[1] == "blob" {
			if size, err := strconv.ParseInt(f[2], 10, 64); err == nil && size > 0 && size <= pointerMax {
				blobs = append(blobs, f[0])
			}
		}
	}
	if len(blobs) == 0 {
		return nil, nil
	}
	body, _, err := g.runStdin(ctx, dir, strings.Join(blobs, "\n")+"\n", "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out2 []Pointer
	for _, content := range batchContents(body) {
		p, ok := parsePointer(content)
		if !ok || seen[p.OID] {
			continue
		}
		seen[p.OID] = true
		out2 = append(out2, p)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].OID < out2[j].OID })
	return out2, nil
}

// batchContents splits `cat-file --batch` output into the objects' bytes.
// Each object comes as "<oid> <type> <size>\n" followed by exactly size
// bytes and a newline.
func batchContents(out string) []string {
	var contents []string
	for rest := out; rest != ""; {
		header, body, ok := strings.Cut(rest, "\n")
		if !ok {
			break
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			break
		}
		size, err := strconv.Atoi(f[2])
		if err != nil || size > len(body) {
			break
		}
		contents = append(contents, body[:size])
		rest = strings.TrimPrefix(body[size:], "\n")
	}
	return contents
}

// parsePointer reads a Git LFS pointer file. The format is fixed: the
// version line first, then the sha256 oid and the size.
func parsePointer(content string) (Pointer, bool) {
	if !strings.HasPrefix(content, "version https://git-lfs.github.com/spec/v1") {
		return Pointer{}, false
	}
	var p Pointer
	for _, line := range strings.Split(content, "\n") {
		switch key, value, _ := strings.Cut(strings.TrimSpace(line), " "); key {
		case "oid":
			hash, oid, ok := strings.Cut(value, ":")
			if !ok || hash != "sha256" || len(oid) != 64 {
				return Pointer{}, false
			}
			p.OID = oid
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return Pointer{}, false
			}
			p.Size = n
		}
	}
	return p, p.OID != ""
}
