package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// Replica states, as stored and shown.
const (
	StateSynced   = "synced"
	StateConflict = "conflict" // some refs need a person; the rest were replicated
	StateError    = "error"
	StateWaiting  = "waiting" // a node involved isn't healthy
	StateMissing  = "missing" // the repository doesn't exist on the replica
)

// Node is a Forgejo node the engine can reach over Git.
type Node struct {
	Name  string
	URL   string // base URL, e.g. https://forgejo-se.example
	User  string // service account
	Token string
}

// Store is the state the engine reads and writes.
type Store interface {
	Repositories(ctx context.Context) ([]store.RepositoryRecord, error)
	Repository(ctx context.Context, id string) (store.RepositoryRecord, error)
	ReplicatedRefs(ctx context.Context, repositoryID, node string) (map[string]string, error)
	SaveReplicaSync(ctx context.Context, st store.ReplicaSync, refs map[string]string) error
	SyncConflicts(ctx context.Context, found []store.FoundConflict, checked, kinds []string, at time.Time) ([]store.ConflictChange, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// HealthSource tells whether nodes are healthy.
type HealthSource interface {
	Snapshot() []health.Status
}

type Options struct {
	Concurrency int // repositories replicated in parallel
	// AfterTriggered, if set, runs after a replication started by Trigger,
	// e.g. to rescan so the inventory reflects the new state right away.
	AfterTriggered func()
}

// Engine replicates repositories that have a primary to all other nodes.
type Engine struct {
	nodes  map[string]Node
	order  []string
	git    *Git
	store  Store
	health HealthSource
	opts   Options
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	running map[string]bool // repository id -> a run is in progress
}

func NewEngine(nodes []Node, git *Git, st Store, h HealthSource, opts Options, log *slog.Logger) *Engine {
	if opts.Concurrency < 1 {
		opts.Concurrency = 2
	}
	e := &Engine{nodes: map[string]Node{}, git: git, store: st, health: h, opts: opts, log: log, now: time.Now, running: map[string]bool{}}
	for _, n := range nodes {
		e.nodes[n.Name] = n
		e.order = append(e.order, n.Name)
	}
	return e
}

// RunAll replicates every repository that has a primary.
func (e *Engine) RunAll(ctx context.Context) {
	recs, err := e.store.Repositories(ctx)
	if err != nil {
		e.log.Error("replication: listing repositories failed", "error", err)
		return
	}
	sem := make(chan struct{}, e.opts.Concurrency)
	var wg sync.WaitGroup
	for _, rec := range recs {
		if rec.PrimaryNode == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := e.RunRepo(ctx, rec); err != nil && !errors.Is(err, errBusy) {
				e.log.Warn("replication failed", "repository", rec.FullName, "error", err)
			}
		}()
	}
	wg.Wait()
}

var errBusy = errors.New("replication of this repository is already running")

// Trigger replicates one repository in the background. It returns false if
// that repository is already being replicated.
func (e *Engine) Trigger(ctx context.Context, id string) (bool, error) {
	rec, err := e.store.Repository(ctx, id)
	if err != nil {
		return false, err
	}
	if rec.PrimaryNode == "" {
		return false, errors.New("the repository has no primary; set one first")
	}
	e.mu.Lock()
	busy := e.running[id]
	e.mu.Unlock()
	if busy {
		return false, nil
	}
	go func() {
		err := e.RunRepo(context.WithoutCancel(ctx), rec)
		if err != nil && !errors.Is(err, errBusy) {
			e.log.Warn("replication failed", "repository", rec.FullName, "error", err)
		}
		if !errors.Is(err, errBusy) && e.opts.AfterTriggered != nil {
			e.opts.AfterTriggered()
		}
	}()
	return true, nil
}

// RunRepo replicates one repository from its primary to every other node.
func (e *Engine) RunRepo(ctx context.Context, rec store.RepositoryRecord) error {
	e.mu.Lock()
	if e.running[rec.ID] {
		e.mu.Unlock()
		return errBusy
	}
	e.running[rec.ID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.running, rec.ID)
		e.mu.Unlock()
	}()

	primary, ok := e.nodes[rec.PrimaryNode]
	if !ok {
		return fmt.Errorf("primary %q isn't a configured node", rec.PrimaryNode)
	}
	replicas := make([]string, 0, len(e.order))
	for _, n := range e.order {
		if n != primary.Name {
			replicas = append(replicas, n)
		}
	}
	healthy := e.healthy()
	record := func(node, state, detail string, updated int, refs map[string]string) {
		st := store.ReplicaSync{RepositoryID: rec.ID, Node: node, State: state, Detail: detail,
			LastAttemptAt: e.now().UTC(), RefsUpdated: updated}
		if err := e.store.SaveReplicaSync(ctx, st, refs); err != nil {
			e.log.Error("replication: saving state failed", "repository", rec.FullName, "node", node, "error", err)
		}
	}
	allReplicas := func(state, detail string) {
		for _, n := range replicas {
			record(n, state, detail, 0, nil)
		}
	}

	if !healthy[primary.Name] {
		allReplicas(StateWaiting, "primary "+primary.Name+" isn't healthy")
		return nil
	}
	dir, err := e.git.Cache(ctx, rec.ID)
	if err != nil {
		allReplicas(StateError, err.Error())
		return err
	}
	pRemote := e.remote(primary, rec.FullName)
	pRefs, err := e.git.LsRemote(ctx, pRemote)
	if err == nil {
		err = e.git.Fetch(ctx, dir, "primary", pRemote)
	}
	if err != nil {
		detail := "reading the primary failed: " + err.Error()
		if errors.Is(err, ErrRepoNotFound) {
			detail = "the repository doesn't exist on the primary " + primary.Name
		}
		allReplicas(StateError, detail)
		return err
	}

	// Conflicts found on each replica, merged per kind and ref.
	type key struct {
		kind IssueKind
		ref  string
	}
	merged := map[key]map[string]string{}
	conclusive := true

	for _, name := range replicas {
		if !healthy[name] {
			record(name, StateWaiting, name+" isn't healthy", 0, nil)
			conclusive = false
			continue
		}
		state, detail, updated, refs, issues, err := e.replicate(ctx, dir, rec, pRefs, e.nodes[name])
		record(name, state, detail, updated, refs)
		if err != nil || state == StateMissing {
			conclusive = false
			continue
		}
		for _, is := range issues {
			k := key{is.Kind, is.Ref}
			if merged[k] == nil {
				merged[k] = map[string]string{}
				if is.Primary != "" {
					merged[k][primary.Name] = is.Primary
				}
			}
			merged[k][name] = is.Replica
		}
	}

	var found []store.FoundConflict
	for k, heads := range merged {
		details := map[string]any{"heads": heads, "primary": primary.Name}
		if name, ok := strings.CutPrefix(k.ref, "refs/heads/"); ok {
			details["branch"] = name
		} else if name, ok := strings.CutPrefix(k.ref, "refs/tags/"); ok {
			details["tag"] = name
		}
		found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: string(k.kind), Ref: k.ref, Details: details})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Kind+found[i].Ref < found[j].Kind+found[j].Ref })
	checked := []string{}
	if conclusive {
		// Every replica was compared: conflicts not found again are resolved.
		checked = []string{rec.ID}
	}
	changes, err := e.store.SyncConflicts(ctx, found, checked, ConflictKinds, e.now().UTC())
	if err != nil {
		return err
	}
	for _, c := range changes {
		e.log.Info("conflict "+c.Change, "repository", c.FullName, "kind", c.Kind, "ref", c.Ref)
		if err := e.store.Audit(ctx, "forgesync", "conflict."+c.Change, c.FullName,
			map[string]any{"conflict_id": c.ID, "kind": c.Kind, "ref": c.Ref}); err != nil {
			e.log.Error("writing audit log failed", "error", err)
		}
	}
	return nil
}

// replicate brings one replica up to date. It returns the state to record,
// the refs ForgeSync now knows to be on the replica (nil = keep the old
// ones), and the issues found.
func (e *Engine) replicate(ctx context.Context, dir string, rec store.RepositoryRecord, pRefs Refs, node Node) (
	state, detail string, updated int, refs map[string]string, issues []Issue, err error) {

	remote := e.remote(node, rec.FullName)
	rRefs, err := e.git.LsRemote(ctx, remote)
	if errors.Is(err, ErrRepoNotFound) {
		return StateMissing, "the repository doesn't exist on " + node.Name + "; create it there to start replicating", 0, nil, nil, nil
	}
	if err == nil && len(rRefs) > 0 {
		// The replica's objects are needed to tell behind from ahead.
		err = e.git.Fetch(ctx, dir, "nodes/"+node.Name, remote)
	}
	if err != nil {
		return StateError, "reading " + node.Name + " failed: " + err.Error(), 0, nil, nil, err
	}
	base, err := e.store.ReplicatedRefs(ctx, rec.ID, node.Name)
	if err != nil {
		return StateError, err.Error(), 0, nil, nil, err
	}

	actions, issues := Plan(pRefs, rRefs, base, func(a, b string) (bool, bool) {
		return e.git.IsAncestor(ctx, dir, a, b)
	})
	results, err := e.git.Push(ctx, dir, remote, actions)
	if err != nil {
		return StateError, "pushing to " + node.Name + " failed: " + err.Error(), 0, nil, issues, err
	}

	// The new base: refs in sync, plus what was just written. Refs with
	// issues or failed pushes keep their old base for the next attempt.
	next := map[string]string{}
	for ref, sha := range base {
		next[ref] = sha
	}
	for ref, p := range pRefs {
		if rRefs[ref] == p {
			next[ref] = p
		}
	}
	for ref := range next {
		if _, inP := pRefs[ref]; !inP {
			if _, inR := rRefs[ref]; !inR {
				delete(next, ref) // gone everywhere
			}
		}
	}
	var failed []string
	for _, a := range actions {
		res := results[a.Ref]
		switch {
		case !res.OK:
			failed = append(failed, a.Ref+": "+res.Reason)
		case a.Kind == Delete:
			delete(next, a.Ref)
			updated++
		default:
			next[a.Ref] = a.Target
			updated++
		}
	}

	switch {
	case len(failed) > 0:
		sort.Strings(failed)
		return StateError, fmt.Sprintf("%d of %d updates rejected by %s: %s", len(failed), len(actions), node.Name,
			strings.Join(failed, "; ")), updated, next, issues, nil
	case len(issues) > 0:
		return StateConflict, fmt.Sprintf("%d ref(s) need a person; see Conflicts", len(issues)), updated, next, issues, nil
	}
	return StateSynced, "", updated, next, nil, nil
}

func (e *Engine) remote(n Node, fullName string) Remote {
	return Remote{URL: strings.TrimRight(n.URL, "/") + "/" + fullName + ".git", User: n.User, Token: n.Token}
}

func (e *Engine) healthy() map[string]bool {
	m := map[string]bool{}
	for _, s := range e.health.Snapshot() {
		m[s.Node] = s.State == health.Healthy
	}
	return m
}
