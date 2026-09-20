package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	// API is the node's REST API, used to create missing repositories. nil
	// leaves them missing.
	API NodeAPI
	// As is the same API acting as another person, for the few writes that
	// belong to one rather than to ForgeSync: publishing a release keeps
	// the name of whoever published it. nil acts as the service account.
	As func(login string) NodeAPI
	// SceneIDSourceID is the id of the SceneID login source on this node
	// (it differs per node). 0 = unknown: owners can't be created here.
	SceneIDSourceID int64
}

// Store is the state the engine reads and writes.
type Store interface {
	Repositories(ctx context.Context) ([]store.RepositoryRecord, error)
	Repository(ctx context.Context, id string) (store.RepositoryRecord, error)
	ReplicatedRefs(ctx context.Context, repositoryID, node string) (map[string]string, error)
	ForgetReplicatedRefs(ctx context.Context, repositoryID, node string) error
	Handoffs(ctx context.Context, repositoryID string, activeOnly bool) ([]store.Handoff, error)
	SaveHandoff(ctx context.Context, h store.Handoff) (int64, error)
	UpdateHandoff(ctx context.Context, h store.Handoff) error
	MarkRepositoryDeleted(ctx context.Context, id string, at time.Time) error
	UndeleteRepository(ctx context.Context, id string) error
	SetRepositoryCollaborators(ctx context.Context, id, value string) error
	SetRepositoryProtection(ctx context.Context, id, value string) error
	SetRepositoryMetadata(ctx context.Context, id string, fields map[string]string, topics string) error
	SetRepositoryReleases(ctx context.Context, id, releases, assets string) error
	SetRepositoryActionVariables(ctx context.Context, id, value string) error
	PackageOwner(ctx context.Context, owner string) (store.PackageOwnerRecord, error)
	SetPackageOwner(ctx context.Context, owner, base string) error
	WikiRefs(ctx context.Context, repositoryID, node string) (map[string]string, error)
	SaveWikiRefs(ctx context.Context, repositoryID, node string, refs map[string]string) error
	Org(ctx context.Context, name string) (store.OrgRecord, error)
	SaveOrg(ctx context.Context, rec store.OrgRecord) error
	DeleteRepository(ctx context.Context, id string) error
	Archives(ctx context.Context, repositoryID string) ([]store.Archive, error)
	SaveArchive(ctx context.Context, a store.Archive) (int64, error)
	RenamedTo(ctx context.Context, repositoryID, node string) (string, error)
	NoteCreatedAccount(ctx context.Context, node, login string) error
	SaveReplicaSync(ctx context.Context, st store.ReplicaSync, refs map[string]string) error
	SyncConflicts(ctx context.Context, found []store.FoundConflict, checked, kinds []string, at time.Time) ([]store.ConflictChange, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// HealthSource tells whether nodes are healthy.
type HealthSource interface {
	Snapshot() []health.Status
}

// Options are what replication copies and what it may do about a
// difference: the switches for each kind of content, the concurrency,
// and whether conflicts are fixed or handed to their owner.
type Options struct {
	Concurrency int // repositories replicated in parallel
	// CreateMissing creates a repository (and its SceneID owner) on a
	// replica that doesn't have it, instead of leaving it missing.
	CreateMissing bool
	// AutoFix applies the fixes that lose nothing: a replica's new commits
	// or refs are taken over by the primary, and a replica's default branch
	// is set to the primary's.
	AutoFix bool
	// HandOff hands diverged branches to the repository's owner as a pull
	// request on the primary (see handoff.go).
	HandOff bool
	// Collaborators keeps the people a repository is shared with the same
	// on every node (collaborators.go).
	Collaborators bool
	// Organizations keeps the organizations that own repositories, their
	// teams and who is in them, the same on every node (orgs.go).
	Organizations bool
	// ProtectReplicas puts ForgeSync's guard on every replica, so people
	// work on the primary and a replica can't be pushed to; it is put back
	// whenever it's removed or changed (protection.go). BranchProtection
	// keeps the owner's own rules the same everywhere.
	ProtectReplicas  bool
	BranchProtection bool
	// Metadata keeps a repository's settings and topics the same on every
	// node (metadata.go).
	Metadata bool
	// Releases keeps what was published on each tag, and the files with
	// it, the same on every node (releases.go). AssetMax is the largest
	// file it carries; default 16 MiB.
	Releases bool
	AssetMax int64
	// Wiki replicates each repository's wiki, which is a second git
	// repository, from its primary to the replicas (wiki.go).
	Wiki bool
	// Packages copies what the nodes' registries hold, for the package
	// types whose files can be fetched and published by path (packages.go).
	// PackageMax is the largest file it carries; 0 means no limit.
	Packages   bool
	PackageMax int64
	// LFS copies the Git LFS objects a repository's pointer files name, so
	// every node that has the repository can check it out (lfs.go). LFSMax
	// is the largest object it carries; 0 means no limit.
	LFS    bool
	LFSMax int64
	// Actions keeps a repository's Actions variables the same on every
	// node, and says which nodes are missing a secret the others have --
	// Forgejo never gives a secret's value back, so no one can copy one
	// (actions.go).
	Actions bool
	// BackupFor is how long ForgeSync keeps what it takes away: a replica's
	// branch after the owner chose the primary's version, and the archived
	// copies of a repository deleted on its primary. Default 30 days.
	BackupFor time.Duration
	// ArchiveOrg is the organization archived copies are moved into.
	// Default "forgesync-archive".
	ArchiveOrg string
	// AfterTriggered, if set, runs after a replication started by Trigger,
	// e.g. to look at the repository again so the inventory reflects the new
	// state right away.
	AfterTriggered func(rec store.RepositoryRecord)
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
	// http fetches and sends LFS objects (lfs.go). Its timeout is per
	// request, and a large object can take a while, so it has none: the
	// run's context bounds it.
	http *http.Client
	now  func() time.Time

	mu      sync.Mutex
	running map[string]bool // repository id -> a run is in progress
	again   map[string]bool // repository id -> changed during the run: run once more
}

// NewEngine replicates each repository from its primary to the other
// nodes, through git and the Forgejo API, recording what it wrote in st.
func NewEngine(nodes []Node, git *Git, st Store, h HealthSource, opts Options, log *slog.Logger) *Engine {
	if opts.Concurrency < 1 {
		opts.Concurrency = 2
	}
	if opts.BackupFor == 0 {
		opts.BackupFor = 30 * 24 * time.Hour
	}
	if opts.ArchiveOrg == "" {
		opts.ArchiveOrg = "forgesync-archive"
	}
	if opts.AssetMax <= 0 {
		opts.AssetMax = 16 << 20
	}
	e := &Engine{nodes: map[string]Node{}, git: git, store: st, health: h, opts: opts, log: log, now: time.Now,
		running: map[string]bool{}, again: map[string]bool{},
		// No timeout: an LFS object can be large, and the run's context
		// already ends the work when leadership or the process does.
		http: &http.Client{}}
	for _, n := range nodes {
		e.nodes[n.Name] = n
		e.order = append(e.order, n.Name)
	}
	return e
}

// RunAll replicates every repository that has a primary, and then brings
// the organizations that own them together.
func (e *Engine) RunAll(ctx context.Context) {
	recs, err := e.store.Repositories(ctx)
	if err != nil {
		e.log.Error("replication: listing repositories failed", "error", err)
		return
	}
	defer func() {
		if ctx.Err() != nil {
			return
		}
		if e.opts.Organizations {
			e.syncOrgs(ctx, recs, e.healthy())
		}
		if e.opts.Packages {
			e.syncPackages(ctx, recs, e.healthy())
		}
	}()
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
// that repository is already being replicated; it then runs once more when
// that run ends, so the change that prompted this isn't missed.
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
	if busy {
		e.again[id] = true
	}
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
			e.opts.AfterTriggered(rec)
		}
	}()
	return true, nil
}

// RunRepo replicates one repository from its primary to every other node.
func (e *Engine) RunRepo(ctx context.Context, rec store.RepositoryRecord) error {
	e.mu.Lock()
	if e.running[rec.ID] {
		e.again[rec.ID] = true // changed while running: once more afterwards
		e.mu.Unlock()
		return errBusy
	}
	e.running[rec.ID] = true
	e.mu.Unlock()
	for {
		err := e.runPasses(ctx, rec)
		e.mu.Lock()
		if !e.again[rec.ID] || ctx.Err() != nil {
			delete(e.running, rec.ID)
			delete(e.again, rec.ID)
			e.mu.Unlock()
			return err
		}
		delete(e.again, rec.ID)
		e.mu.Unlock()
		if fresh, ferr := e.store.Repository(ctx, rec.ID); ferr == nil {
			rec = fresh
		} else if errors.Is(ferr, store.ErrNotFound) {
			e.mu.Lock()
			delete(e.running, rec.ID)
			e.mu.Unlock()
			return nil // forgotten meanwhile (e.g. a deleted repository's last archive went)
		}
	}
}

func (e *Engine) runPasses(ctx context.Context, rec store.RepositoryRecord) error {
	// A fix can move the primary (e.g. taking a replica's new commits); a
	// second pass then carries that to the other replicas right away.
	for pass := 0; pass < 2; pass++ {
		again, err := e.runOnce(ctx, rec)
		if err != nil || !again {
			return err
		}
	}
	return nil
}

// runOnce replicates one repository from its primary to every other node,
// applies owners' decisions and safe fixes, and hands diverged branches to the
// owner. again reports that the primary changed, so another pass is useful.
func (e *Engine) runOnce(ctx context.Context, rec store.RepositoryRecord) (again bool, err error) {
	primary, ok := e.nodes[rec.PrimaryNode]
	if !ok {
		return false, fmt.Errorf("primary %q isn't a configured node", rec.PrimaryNode)
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

	// Archived copies whose backup period is over go first; once nothing of
	// a deleted repository is left, ForgeSync forgets it.
	if gone, err := e.purgeArchives(ctx, rec, healthy); err != nil {
		return false, err
	} else if gone {
		if err := e.store.DeleteRepository(ctx, rec.ID); err != nil {
			return false, err
		}
		e.log.Info("deleted repository forgotten", "repository", rec.FullName)
		e.audit(ctx, "repo.forgotten", rec.FullName, map[string]any{"repository_id": rec.ID})
		return false, nil
	}
	if !healthy[primary.Name] {
		allReplicas(StateWaiting, "primary "+primary.Name+" isn't healthy")
		return false, nil
	}
	dir, err := e.git.Cache(ctx, rec.ID)
	if err != nil {
		allReplicas(StateError, err.Error())
		return false, err
	}
	pRemote := e.remote(primary, rec.FullName)
	pRefs, err := e.git.LsRemote(ctx, pRemote)
	if errors.Is(err, ErrRepoNotFound) {
		had, gone := primaryHad(rec, primary.Name)
		if had && gone && rec.DeletedAt == nil {
			// Renamed or transferred, not deleted, if the primary has it
			// under another name; the next scan round applies that.
			to, err := e.store.RenamedTo(ctx, rec.ID, primary.Name)
			if err != nil {
				return false, err
			}
			if to != "" {
				allReplicas(StateWaiting, "renamed on the primary "+primary.Name+" to "+to+"; applied after the next scan")
				return false, nil
			}
		}
		switch {
		case had && gone:
			_, err := e.deletedOnPrimary(ctx, rec, primary, replicas, healthy, record)
			return false, err
		case had:
			// Git says it's gone, but the latest scan still listed it.
			allReplicas(StateWaiting, "the repository is gone from the primary "+primary.Name+
				"; waiting for the next scan to confirm it was deleted")
			return false, nil
		}
	}
	if err == nil && rec.DeletedAt != nil {
		// Created again on the primary: a normal repository again.
		if err := e.store.UndeleteRepository(ctx, rec.ID); err != nil {
			return false, err
		}
		e.log.Info("deleted repository created again on its primary", "repository", rec.FullName)
		e.audit(ctx, "repo.recreated_on_primary", rec.FullName, map[string]any{"repository_id": rec.ID, "primary": primary.Name})
	}
	if errors.Is(err, ErrRepoNotFound) && e.opts.CreateMissing {
		// Only a primary that never had it gets here.
		// The primary follows the owner, so it may not have the repository
		// yet (e.g. created on SE by a user whose primary site is DK).
		var why blocked
		switch serr := e.seedPrimary(ctx, dir, rec, primary, healthy); {
		case errors.As(serr, &why):
			allReplicas(StateMissing, "the primary "+primary.Name+" doesn't have the repository yet: "+why.Error())
			return false, nil
		case serr != nil:
			allReplicas(StateError, "copying the repository to the primary "+primary.Name+" failed: "+serr.Error())
			return false, serr
		}
		pRefs, err = e.git.LsRemote(ctx, pRemote)
	}
	if err == nil {
		err = e.git.Fetch(ctx, dir, "primary", pRemote)
	}
	if err != nil {
		detail := "reading the primary failed: " + err.Error()
		if errors.Is(err, ErrRepoNotFound) {
			detail = "the repository doesn't exist on the primary " + primary.Name
		}
		allReplicas(StateError, detail)
		return false, err
	}
	// Hand-off branches live on the primary only.
	pRefs, handoffRefs := splitHandoffRefs(pRefs)

	var handoffs []store.Handoff
	if e.opts.HandOff {
		if handoffs, err = e.store.Handoffs(ctx, rec.ID, true); err != nil {
			return false, err
		}
		// Owners' decisions first, so this run already compares the result.
		handoffs = e.applyDecisions(ctx, dir, rec, primary, pRefs, handoffRefs, handoffs, healthy)
	}

	// Conflicts found on each replica, merged per kind and ref.
	type key struct {
		kind IssueKind
		ref  string
	}
	merged := map[key]map[string]string{}
	conclusive := true
	var diverged []divergence

	for _, name := range replicas {
		if !healthy[name] {
			record(name, StateWaiting, name+" isn't healthy", 0, nil)
			conclusive = false
			continue
		}
		// A rename on the primary is applied to this copy first, so a new
		// copy isn't created next to it.
		if old := pendingRename(rec, name); old != "" {
			var why blocked
			switch err := e.renameOn(ctx, rec, primary, e.nodes[name], old); {
			case errors.As(err, &why):
				record(name, StateMissing, why.Error(), 0, nil)
				conclusive = false
				continue
			case err != nil:
				record(name, StateError, "renaming the copy on "+name+" failed: "+err.Error(), 0, nil)
				conclusive = false
				continue
			}
		}
		state, detail, updated, refs, issues, err := e.replicate(ctx, dir, rec, pRefs, primary, e.nodes[name])
		if err == nil && len(issues) > 0 && e.opts.AutoFix {
			var fixed int
			issues, fixed = e.autoFix(ctx, dir, rec, primary, name, pRefs, issues)
			if fixed > 0 {
				again = true
				if len(issues) == 0 && state == StateConflict {
					state, detail = StateSynced, fmt.Sprintf("%d ref(s) taken over by the primary; they replicate from there", fixed)
				}
			}
		}
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
			if handOffKind(is) {
				diverged = append(diverged, divergence{node: name, issue: is})
			}
		}
	}
	if e.opts.HandOff && !again {
		// Hand off only once the primary has settled in this run.
		handoffs = e.handOff(ctx, dir, rec, primary, pRefs, handoffRefs, handoffs, diverged)
	}

	var found []store.FoundConflict
	for k, heads := range merged {
		details := map[string]any{"heads": heads, "primary": primary.Name}
		if name, ok := strings.CutPrefix(k.ref, "refs/heads/"); ok {
			details["branch"] = name
		} else if name, ok := strings.CutPrefix(k.ref, "refs/tags/"); ok {
			details["tag"] = name
		}
		if hs := handoffsFor(handoffs, k.ref, heads); len(hs) > 0 {
			details["handoffs"] = hs
		}
		found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: string(k.kind), Ref: k.ref, Details: details})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Kind+found[i].Ref < found[j].Kind+found[j].Ref })
	checked := []string{}
	if conclusive {
		// Every replica was compared: conflicts not found again are resolved.
		checked = []string{rec.ID}
	}
	if err := e.syncConflicts(ctx, rec, found, checked); err != nil {
		return again, err
	}
	if e.opts.AutoFix && !again {
		e.fixDefaultBranches(ctx, rec, primary, healthy)
	}
	if e.opts.Collaborators && !again {
		e.syncCollaborators(ctx, rec, primary, healthy)
	}
	if e.opts.Metadata && !again {
		e.syncMetadata(ctx, rec, healthy)
	}
	if e.opts.Releases && !again {
		e.syncReleases(ctx, rec, healthy)
	}
	if e.opts.Wiki && !again {
		e.syncWiki(ctx, rec, primary, healthy)
	}
	if e.opts.Actions && !again {
		e.syncActions(ctx, rec, healthy)
	}
	if e.opts.LFS && !again {
		e.syncLFS(ctx, dir, rec, healthy)
	}
	if !again {
		// The owner's rules first, so the guard isn't mistaken for one.
		if e.opts.BranchProtection {
			e.syncProtection(ctx, rec, healthy)
		}
		if e.opts.ProtectReplicas {
			e.protectReplicas(ctx, rec, primary, healthy)
		}
	}
	return again, nil
}

// replicate brings one replica up to date. It returns the state to record,
// the refs ForgeSync now knows to be on the replica (nil = keep the old
// ones), and the issues found.
func (e *Engine) replicate(ctx context.Context, dir string, rec store.RepositoryRecord, pRefs Refs, primary, node Node) (
	state, detail string, updated int, refs map[string]string, issues []Issue, err error) {

	remote := e.remote(node, rec.FullName)
	rRefs, err := e.git.LsRemote(ctx, remote)
	if errors.Is(err, ErrRepoNotFound) {
		if !e.opts.CreateMissing {
			return StateMissing, "the repository doesn't exist on " + node.Name + "; create it there to start replicating", 0, nil, nil, nil
		}
		var why blocked
		switch err := e.createOnReplica(ctx, rec.FullName, primary, node); {
		case errors.As(err, &why):
			return StateMissing, why.Error(), 0, nil, nil, nil
		case err != nil:
			return StateError, "creating the repository on " + node.Name + " failed: " + err.Error(), 0, nil, nil, err
		}
		// Anything ForgeSync wrote to an earlier copy that was deleted is gone.
		if err := e.store.ForgetReplicatedRefs(ctx, rec.ID, node.Name); err != nil {
			return StateError, err.Error(), 0, nil, nil, err
		}
		rRefs, err = e.git.LsRemote(ctx, remote)
	}
	rRefs, _ = splitHandoffRefs(rRefs) // not ForgeSync's to replicate
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
	// A deletion needs ForgeSync's own guard lifted: Forgejo won't let
	// anyone delete a protected branch. Everything else is a create or a
	// fast-forward, which the guard lets a site admin through.
	deleting := false
	for _, a := range actions {
		if a.Kind == Delete {
			deleting = true
		}
	}
	var results map[string]PushResult
	if deleting {
		err = e.withoutGuard(ctx, rec, node, func() (err error) {
			results, err = e.git.Push(ctx, dir, remote, actions)
			return err
		})
	} else {
		results, err = e.git.Push(ctx, dir, remote, actions)
	}
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
	// A replica that refuses ForgeSync's push has almost certainly had the
	// guard's whitelist emptied; it can't be read back, so a refusal is
	// how ForgeSync finds out. What git reports per ref is the porcelain
	// status -- "pre-receive hook declined" -- not the message Forgejo
	// prints beside it, so that is what's matched. Writing the guard again
	// is one call and changes nothing when it was already right; the next
	// run pushes again.
	if e.opts.ProtectReplicas && node.Name != rec.PrimaryNode {
		for _, res := range results {
			if !res.OK && (strings.Contains(res.Reason, "pre-receive hook declined") ||
				strings.Contains(res.Reason, "protected branch")) {
				e.reassertGuard(ctx, rec, node)
				break
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

// seedPrimary copies a repository to its primary when the primary doesn't
// have it: it creates it there (with its owner, as for any replica) and
// pushes every branch and tag from the node where it was created first. The
// new copy is empty, so this only creates refs.
func (e *Engine) seedPrimary(ctx context.Context, dir string, rec store.RepositoryRecord, primary Node, healthy map[string]bool) error {
	var src *store.Replica
	for i, r := range rec.Replicas {
		if !r.Present || r.Mirror || r.Node == primary.Name || !healthy[r.Node] || r.ForgejoCreated == nil {
			continue
		}
		if _, ok := e.nodes[r.Node]; ok && (src == nil || r.ForgejoCreated.Before(*src.ForgejoCreated)) {
			src = &rec.Replicas[i]
		}
	}
	if src == nil {
		return blocked("no healthy node has a copy to start from")
	}
	from := e.nodes[src.Node]
	if err := e.createOnReplica(ctx, rec.FullName, from, primary); err != nil {
		return err
	}
	sRemote := e.remote(from, rec.FullName)
	sRefs, err := e.git.LsRemote(ctx, sRemote)
	if err != nil {
		return err
	}
	if len(sRefs) == 0 {
		return nil // an empty repository: nothing to copy
	}
	if err := e.git.Fetch(ctx, dir, "nodes/"+from.Name, sRemote); err != nil {
		return err
	}
	pRemote := e.remote(primary, rec.FullName)
	pRefs, err := e.git.LsRemote(ctx, pRemote)
	if err != nil {
		return err
	}
	actions, _ := Plan(sRefs, pRefs, nil, func(a, b string) (bool, bool) { return e.git.IsAncestor(ctx, dir, a, b) })
	var creates []Action
	for _, a := range actions {
		if a.Kind == Create { // never anything else on the primary
			creates = append(creates, a)
		}
	}
	results, err := e.git.Push(ctx, dir, pRemote, creates)
	if err != nil {
		return err
	}
	for _, a := range creates {
		if r := results[a.Ref]; !r.OK {
			return fmt.Errorf("%s refused %s: %s", primary.Name, a.Ref, r.Reason)
		}
	}
	e.log.Info("copied repository to its primary", "repository", rec.FullName, "from", from.Name, "to", primary.Name, "refs", len(creates))
	return nil
}

// syncConflicts stores the git conflicts found for a repository and audits
// what opened or cleared.
func (e *Engine) syncConflicts(ctx context.Context, rec store.RepositoryRecord, found []store.FoundConflict, checked []string) error {
	changes, err := e.store.SyncConflicts(ctx, found, checked, ConflictKinds, e.now().UTC())
	if err != nil {
		return err
	}
	for _, c := range changes {
		e.log.Info("conflict "+c.Change, "repository", c.FullName, "kind", c.Kind, "ref", c.Ref)
		e.audit(ctx, "conflict."+c.Change, c.FullName, map[string]any{"conflict_id": c.ID, "kind": c.Kind, "ref": c.Ref})
	}
	return nil
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
