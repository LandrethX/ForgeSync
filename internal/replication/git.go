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
