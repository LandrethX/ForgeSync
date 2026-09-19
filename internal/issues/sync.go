package issues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// API is the part of the Forgejo REST API issue replication uses.
type API interface {
	ListIssues(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.Issue, error)
	ListRepoComments(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.IssueComment, error)
	CreateIssue(ctx context.Context, owner, repo, title, body string, closed bool, labels []int64, milestone int64,
		assignees []string) (forgejo.Issue, error)
	SetIssueMilestone(ctx context.Context, owner, repo string, number, milestone int64) error
	SetIssueAssignees(ctx context.Context, owner, repo string, number int64, logins []string) error
	IssueReactions(ctx context.Context, owner, repo string, number int64, page, limit int) ([]forgejo.Reaction, error)
	CommentReactions(ctx context.Context, owner, repo string, id int64, page, limit int) ([]forgejo.Reaction, error)
	AddIssueReaction(ctx context.Context, owner, repo string, number int64, content string) error
	RemoveIssueReaction(ctx context.Context, owner, repo string, number int64, content string) error
	AddCommentReaction(ctx context.Context, owner, repo string, id int64, content string) error
	RemoveCommentReaction(ctx context.Context, owner, repo string, id int64, content string) error
	IssueAttachments(ctx context.Context, owner, repo string, number int64) ([]forgejo.Attachment, error)
	CommentAttachments(ctx context.Context, owner, repo string, id int64) ([]forgejo.Attachment, error)
	UploadIssueAttachment(ctx context.Context, owner, repo string, number int64, name string, content []byte) (forgejo.Attachment, error)
	UploadCommentAttachment(ctx context.Context, owner, repo string, id int64, name string, content []byte) (forgejo.Attachment, error)
	DeleteIssueAttachment(ctx context.Context, owner, repo string, number, id int64) error
	DeleteCommentAttachment(ctx context.Context, owner, repo string, commentID, id int64) error
	Download(ctx context.Context, url string, max int64) ([]byte, error)
	ListAssignees(ctx context.Context, owner, repo string) ([]forgejo.User, error)
	ReplaceIssueLabels(ctx context.Context, owner, repo string, number int64, labels []int64) error
	ListLabels(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.Label, error)
	CreateLabel(ctx context.Context, owner, repo string, l forgejo.Label) (forgejo.Label, error)
	EditLabel(ctx context.Context, owner, repo string, id int64, fields map[string]any) error
	DeleteLabel(ctx context.Context, owner, repo string, id int64) error
	ListMilestones(ctx context.Context, owner, repo string, page, limit int) ([]forgejo.Milestone, error)
	CreateMilestone(ctx context.Context, owner, repo string, m forgejo.Milestone) (forgejo.Milestone, error)
	EditMilestone(ctx context.Context, owner, repo string, id int64, fields map[string]any) error
	DeleteMilestone(ctx context.Context, owner, repo string, id int64) error
	EditIssue(ctx context.Context, owner, repo string, number int64, title, body, state *string) error
	DeleteIssue(ctx context.Context, owner, repo string, number int64) error
	CreateIssueComment(ctx context.Context, owner, repo string, number int64, body string) (forgejo.IssueComment, error)
	EditIssueComment(ctx context.Context, owner, repo string, id int64, body string) error
	DeleteIssueComment(ctx context.Context, owner, repo string, id int64) error
}

// Node is a Forgejo node: its API as the service account, and as a user.
type Node struct {
	Name string
	API  API
	As   func(login string) API
}

// Store is what issue replication reads and writes.
type Store interface {
	Repositories(ctx context.Context) ([]store.RepositoryRecord, error)
	Repository(ctx context.Context, id string) (store.RepositoryRecord, error)
	Issues(ctx context.Context, repositoryID string) ([]store.IssueRecord, []store.CommentRecord, error)
	SaveIssue(ctx context.Context, r store.IssueRecord) (string, error)
	DeleteIssueRecord(ctx context.Context, id string) error
	SaveComment(ctx context.Context, c store.CommentRecord) (string, error)
	DeleteCommentRecord(ctx context.Context, id string) error
	RepoItems(ctx context.Context, repositoryID string) ([]store.RepoItem, error)
	SaveRepoItem(ctx context.Context, it store.RepoItem) (string, error)
	DeleteRepoItem(ctx context.Context, id string) error
	SyncConflicts(ctx context.Context, found []store.FoundConflict, checked, kinds []string, at time.Time) ([]store.ConflictChange, error)
	Audit(ctx context.Context, actor, action, target string, details map[string]any) error
}

// HealthSource tells whether nodes are healthy.
type HealthSource interface {
	Snapshot() []health.Status
}

// ConflictKind is the conflict kind issue replication owns.
const ConflictKind = "issue_conflict"

type Options struct {
	Concurrency int // repositories in parallel
	// Reactions also replicates the reactions on issues and comments.
	// Forgejo has no bulk endpoint for them, so it costs one call per issue
	// and per comment per node on every run; off by default.
	Reactions bool
	// Attachments also replicates the files on issues and comments, which
	// costs the same listing plus a download and an upload for each file
	// that has to move. AttachmentMax is the largest file it will carry
	// (default 16 MiB); a bigger one is left where it is, with a warning.
	Attachments   bool
	AttachmentMax int64
	// EnsureUser makes an author exist on a node as on another, as for
	// repository owners (replication.Engine.EnsureUser). nil: authors must
	// exist already.
	EnsureUser func(ctx context.Context, login, from, to string) error
}

// defaultAttachmentMax is the largest file replication carries by default.
const defaultAttachmentMax = 16 << 20

// Syncer replicates the issues of repositories that have a primary.
type Syncer struct {
	nodes  map[string]Node
	order  []string
	store  Store
	health HealthSource
	opts   Options
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	running map[string]bool
	again   map[string]bool
}

func NewSyncer(nodes []Node, st Store, h HealthSource, opts Options, log *slog.Logger) *Syncer {
	if opts.Concurrency < 1 {
		opts.Concurrency = 2
	}
	if opts.AttachmentMax <= 0 {
		opts.AttachmentMax = defaultAttachmentMax
	}
	s := &Syncer{nodes: map[string]Node{}, store: st, health: h, opts: opts, log: log, now: time.Now,
		running: map[string]bool{}, again: map[string]bool{}}
	for _, n := range nodes {
		s.nodes[n.Name] = n
		s.order = append(s.order, n.Name)
	}
	return s
}

// RunAll replicates the issues of every repository with a primary.
func (s *Syncer) RunAll(ctx context.Context) {
	recs, err := s.store.Repositories(ctx)
	if err != nil {
		s.log.Error("issues: listing repositories failed", "error", err)
		return
	}
	sem := make(chan struct{}, s.opts.Concurrency)
	var wg sync.WaitGroup
	for _, rec := range recs {
		if rec.PrimaryNode == "" || rec.DeletedAt != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := s.RunRepo(ctx, rec.ID); err != nil {
				s.log.Warn("issue replication failed", "repository", rec.FullName, "error", err)
			}
		}()
	}
	wg.Wait()
}

// Trigger replicates one repository's issues in the background; a trigger
// while it runs makes it run once more afterwards.
func (s *Syncer) Trigger(ctx context.Context, id string) {
	go func() {
		if err := s.RunRepo(context.WithoutCancel(ctx), id); err != nil {
			s.log.Warn("issue replication failed", "repository", id, "error", err)
		}
	}()
}

// RunRepo replicates one repository's issues and comments.
func (s *Syncer) RunRepo(ctx context.Context, id string) error {
	s.mu.Lock()
	if s.running[id] {
		s.again[id] = true
		s.mu.Unlock()
		return nil
	}
	s.running[id] = true
	s.mu.Unlock()
	for {
		err := s.runOnce(ctx, id)
		s.mu.Lock()
		if !s.again[id] || ctx.Err() != nil || err != nil {
			delete(s.running, id)
			delete(s.again, id)
			s.mu.Unlock()
			return err
		}
		delete(s.again, id)
		s.mu.Unlock()
	}
}

const pageSize = 50

// snapshot is what one node has of a repository's issues and comments.
type snapshot struct {
	issues   map[int64]forgejo.Issue                // by Forgejo id
	byNumber map[int64]forgejo.Issue                // issues only, by number
	comments map[int64]forgejo.IssueComment         // on issues, by Forgejo id
	items    map[string]map[int64]map[string]string // kind -> Forgejo id -> fields
	// assignable are the logins the node lets an issue be assigned to:
	// those with write access to the repository there.
	assignable map[string]bool
	// reactions and commentReactions are "<login>:<content>" sets, by the
	// issue's and the comment's Forgejo id. Empty unless Options.Reactions.
	reactions        map[int64]map[string]bool
	commentReactions map[int64]map[string]bool
	// attachments and commentAttachments are the files by "<size>:<name>",
	// likewise. Empty unless Options.Attachments.
	attachments        map[int64]map[string]forgejo.Attachment
	commentAttachments map[int64]map[string]forgejo.Attachment
}

func (s *Syncer) read(ctx context.Context, n Node, owner, name string) (*snapshot, error) {
	sn := &snapshot{issues: map[int64]forgejo.Issue{}, byNumber: map[int64]forgejo.Issue{}, comments: map[int64]forgejo.IssueComment{},
		items: map[string]map[int64]map[string]string{"label": {}, "milestone": {}}, assignable: map[string]bool{}}
	who, err := n.API.ListAssignees(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	for _, u := range who {
		sn.assignable[u.Login] = true
	}
	for page := 1; ; page++ {
		list, err := n.API.ListLabels(ctx, owner, name, page, pageSize)
		if err != nil {
			return nil, err
		}
		for _, l := range list {
			sn.items["label"][l.ID] = labelFields(l)
		}
		if len(list) < pageSize {
			break
		}
	}
	for page := 1; ; page++ {
		list, err := n.API.ListMilestones(ctx, owner, name, page, pageSize)
		if err != nil {
			return nil, err
		}
		for _, m := range list {
			sn.items["milestone"][m.ID] = milestoneFields(m)
		}
		if len(list) < pageSize {
			break
		}
	}
	for page := 1; ; page++ {
		list, err := n.API.ListIssues(ctx, owner, name, page, pageSize)
		if err != nil {
			return nil, err
		}
		for _, is := range list {
			if is.PullRequest == nil {
				sn.issues[is.ID], sn.byNumber[is.Number] = is, is
			}
		}
		if len(list) < pageSize {
			break
		}
	}
	for page := 1; ; page++ {
		list, err := n.API.ListRepoComments(ctx, owner, name, page, pageSize)
		if err != nil {
			return nil, err
		}
		for _, c := range list {
			if _, onIssue := sn.byNumber[c.IssueNumber()]; onIssue && c.PRURL == "" {
				sn.comments[c.ID] = c
			}
		}
		if len(list) < pageSize {
			break
		}
	}
	if s.opts.Reactions || s.opts.Attachments {
		if err := s.readExtras(ctx, n, owner, name, sn); err != nil {
			return nil, err
		}
	}
	return sn, nil
}

// readExtras adds every issue's and comment's reactions and attachments to
// the snapshot. Forgejo has no bulk endpoint for either, so this is one
// call each; they go out a few at a time.
func (s *Syncer) readExtras(ctx context.Context, n Node, owner, name string, sn *snapshot) error {
	sn.reactions = make(map[int64]map[string]bool, len(sn.issues))
	sn.commentReactions = make(map[int64]map[string]bool, len(sn.comments))
	sn.attachments = make(map[int64]map[string]forgejo.Attachment, len(sn.issues))
	sn.commentAttachments = make(map[int64]map[string]forgejo.Attachment, len(sn.comments))
	type job struct {
		id      int64 // the issue's or the comment's Forgejo id
		number  int64 // the issue's number, 0 for a comment
		comment bool
	}
	jobs := make([]job, 0, len(sn.issues)+len(sn.comments))
	for id, is := range sn.issues {
		jobs = append(jobs, job{id: id, number: is.Number})
	}
	for id := range sn.comments {
		jobs = append(jobs, job{id: id, comment: true})
	}
	workers := min(max(s.opts.Concurrency, 4), len(jobs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var first error
	next := make(chan job)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range next {
				var set map[string]bool
				var files map[string]forgejo.Attachment
				var err error
				if s.opts.Reactions {
					set, err = s.reactionsOf(ctx, n, owner, name, j.id, j.number, j.comment)
				}
				if err == nil && s.opts.Attachments {
					files, err = s.attachmentsOf(ctx, n, owner, name, j.id, j.number, j.comment)
				}
				mu.Lock()
				switch {
				case err != nil:
					if first == nil {
						first = err
					}
				case j.comment:
					sn.commentReactions[j.id], sn.commentAttachments[j.id] = set, files
				default:
					sn.reactions[j.id], sn.attachments[j.id] = set, files
				}
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		next <- j
	}
	close(next)
	wg.Wait()
	return first
}

func (s *Syncer) reactionsOf(ctx context.Context, n Node, owner, name string, id, number int64, comment bool) (map[string]bool, error) {
	set := map[string]bool{}
	for page := 1; ; page++ {
		var list []forgejo.Reaction
		var err error
		if comment {
			list, err = n.API.CommentReactions(ctx, owner, name, id, page, pageSize)
		} else {
			list, err = n.API.IssueReactions(ctx, owner, name, number, page, pageSize)
		}
		if err != nil {
			return nil, err
		}
		for e := range reactionSet(list) {
			set[e] = true
		}
		if len(list) < pageSize {
			return set, nil
		}
	}
}

func (s *Syncer) attachmentsOf(ctx context.Context, n Node, owner, name string, id, number int64, comment bool) (map[string]forgejo.Attachment, error) {
	var list []forgejo.Attachment
	var err error
	if comment {
		list, err = n.API.CommentAttachments(ctx, owner, name, id)
	} else {
		list, err = n.API.IssueAttachments(ctx, owner, name, number)
	}
	if err != nil {
		return nil, err
	}
	return attachmentsOn(list), nil
}

// run is one repository's replication pass.
type run struct {
	s           *Syncer
	rec         store.RepositoryRecord
	owner, name string
	primary     string
	nodes       []string // taking part: healthy and readable, primary first
	snaps       map[string]*snapshot
	found       []store.FoundConflict
	complete    bool
	labels      itemIndex
	milestones  itemIndex
	// restored are the nodes where a label or milestone deleted there is
	// being recreated in this run; see itemVanished.
	restored map[string]bool
	// kept is, per node, the labels and milestones deleted on the primary
	// whose changed copy stays there for its owner; see itemKept.
	kept map[string]map[string]bool
}

// labelsValue is an issue's labels on a node, as merge sees them there.
func (r *run) labelsValue(node string, is forgejo.Issue) string {
	return r.labels.labelsValue(node, is, r.kept[node])
}

// assigneesValue is an issue's assignees as merge sees them: their logins,
// sorted. A login means the same person on every node, since SceneID's sub
// is the global user key, so there's nothing to translate.
func assigneesValue(_ string, is forgejo.Issue) string {
	logins := make([]string, 0, len(is.Assignees))
	for _, u := range is.Assignees {
		logins = append(logins, u.Login)
	}
	sort.Strings(logins)
	return strings.Join(logins, ",")
}

// refused lists, as "<node>: <reason>", the nodes that couldn't hold this
// value. Nothing is written while one can't, and that includes a node
// without a copy of the issue yet: its copy is made without the value, so
// the base mustn't move past it either.
func (r *run) refused(refuse func(node, value string) string, value string) []string {
	if refuse == nil {
		return nil
	}
	var out []string
	for _, n := range r.nodes {
		if why := refuse(n, value); why != "" {
			out = append(out, n+": "+why)
		}
	}
	return out
}

// refuseAssignees says why a node wouldn't take these assignees.
func (r *run) refuseAssignees(node, value string) string {
	var missing []string
	for _, login := range split(value) {
		if !r.snaps[node].assignable[login] {
			missing = append(missing, login)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return strings.Join(missing, ", ") + " can't be given an issue there"
}

// split is a merged list value as its parts; "" is no parts.
func split(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

// canAssign reports that the node would take every assignee in value.
// Forgejo only assigns people with write access to the repository, and
// ForgeSync doesn't replicate collaborators yet, so a node can be unable to
// hold what another node has. It's never given a value it can't hold, and
// its own value is then not read as anyone's decision either (see skip).
func (r *run) canAssign(node, value string) bool {
	if value == "" {
		return true
	}
	for _, login := range split(value) {
		if !r.snaps[node].assignable[login] {
			return false
		}
	}
	return true
}

func (s *Syncer) runOnce(ctx context.Context, id string) error {
	rec, err := s.store.Repository(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.PrimaryNode == "" || rec.DeletedAt != nil {
		return nil
	}
	healthy := map[string]bool{}
	for _, st := range s.health.Snapshot() {
		healthy[st.Node] = st.State == health.Healthy
	}
	if !healthy[rec.PrimaryNode] {
		return nil // the next run, once it's back
	}
	owner, name, _ := strings.Cut(rec.FullName, "/")
	r := &run{s: s, rec: rec, owner: owner, name: name, primary: rec.PrimaryNode, snaps: map[string]*snapshot{}, complete: true}
	ordered := append([]string{rec.PrimaryNode}, s.order...)
	for _, n := range ordered {
		if _, seen := r.snaps[n]; seen || !healthy[n] {
			if !healthy[n] {
				r.complete = false
			}
			continue
		}
		sn, err := s.read(ctx, s.nodes[n], owner, name)
		if err != nil {
			if n == rec.PrimaryNode {
				return fmt.Errorf("reading issues on the primary %s: %w", n, err)
			}
			// E.g. the repository isn't on that node yet.
			s.log.Debug("issues: node skipped", "repository", rec.FullName, "node", n, "error", err)
			r.complete = false
			continue
		}
		r.snaps[n] = sn
		r.nodes = append(r.nodes, n)
	}

	// Labels and milestones first: issues refer to them.
	items, err := s.store.RepoItems(ctx, id)
	if err != nil {
		return err
	}
	byKind := map[string][]store.RepoItem{}
	for _, it := range items {
		byKind[it.Kind] = append(byKind[it.Kind], it)
	}
	labels, err := r.items(ctx, labelKind, byKind["label"])
	if err != nil {
		return err
	}
	milestones, err := r.items(ctx, milestoneKind, byKind["milestone"])
	if err != nil {
		return err
	}
	r.labels, r.milestones = newItemIndex(labels), newItemIndex(milestones)

	issues, comments, err := s.store.Issues(ctx, id)
	if err != nil {
		return err
	}
	issues, err = r.issues(ctx, issues)
	if err != nil {
		return err
	}
	if err := r.comments(ctx, issues, comments); err != nil {
		return err
	}
	checked := []string{}
	if r.complete {
		checked = []string{id}
	}
	changes, err := s.store.SyncConflicts(ctx, r.found, checked, []string{ConflictKind}, s.now().UTC())
	if err != nil {
		return err
	}
	for _, c := range changes {
		s.log.Info("conflict "+c.Change, "repository", c.FullName, "kind", c.Kind, "ref", c.Ref)
		if err := s.store.Audit(ctx, "forgesync", "conflict."+c.Change, c.FullName,
			map[string]any{"conflict_id": c.ID, "kind": c.Kind, "ref": c.Ref}); err != nil {
			s.log.Error("writing audit log failed", "error", err)
		}
	}
	return nil
}

func (r *run) conflict(ref string, details map[string]any) {
	r.found = append(r.found, store.FoundConflict{RepositoryID: r.rec.ID, Kind: ConflictKind, Ref: ref, Details: details})
}

// issueRef names an issue in conflicts: its number on the primary if it has
// one there.
func issueRef(rec store.IssueRecord, primary string) string {
	if c, ok := rec.Copies[primary]; ok {
		return fmt.Sprintf("#%d", c.Number)
	}
	for _, c := range rec.Copies {
		return fmt.Sprintf("#%d", c.Number)
	}
	return rec.ID
}

func (r *run) issues(ctx context.Context, recs []store.IssueRecord) ([]store.IssueRecord, error) {
	// Index the copies ForgeSync knows, then attach or create records for
	// issues it doesn't know: an issue with the same author and title as a
	// record lacking a copy on that node is that copy (identical issues made
	// before replication, or a copy whose recording was interrupted).
	known := map[string]map[int64]bool{}
	for _, n := range r.nodes {
		known[n] = map[int64]bool{}
	}
	for _, rec := range recs {
		for n, c := range rec.Copies {
			if known[n] != nil {
				known[n][c.ForgejoID] = true
			}
		}
	}
	type fresh struct {
		node string
		is   forgejo.Issue
	}
	var news []fresh
	for _, n := range r.nodes {
		for _, is := range r.snaps[n].issues {
			if !known[n][is.ID] {
				news = append(news, fresh{n, is})
			}
		}
	}
	sort.Slice(news, func(i, j int) bool {
		if !news[i].is.Created.Equal(news[j].is.Created) {
			return news[i].is.Created.Before(news[j].is.Created)
		}
		if news[i].node != news[j].node {
			return news[i].node == r.primary || (news[j].node != r.primary && news[i].node < news[j].node)
		}
		return news[i].is.Number < news[j].is.Number
	})
	for _, f := range news {
		attached := false
		for i := range recs {
			rec := &recs[i]
			if _, has := rec.Copies[f.node]; has || !strings.EqualFold(rec.Author, f.is.User.Login) || r.currentTitle(*rec) != f.is.Title {
				continue
			}
			rec.Copies[f.node] = store.IssueCopy{Number: f.is.Number, ForgejoID: f.is.ID}
			if f.node == r.primary {
				rec.DeletedAt = nil // recreated on the primary to keep it
			}
			if _, err := r.s.store.SaveIssue(ctx, *rec); err != nil {
				return nil, err
			}
			attached = true
			break
		}
		if attached {
			continue
		}
		rec := store.IssueRecord{RepositoryID: r.rec.ID, OriginNode: f.node, Author: f.is.User.Login, CreatedAt: f.is.Created,
			BaseTitle: f.is.Title, BaseBody: f.is.Body, BaseState: f.is.State,
			Copies: map[string]store.IssueCopy{f.node: {Number: f.is.Number, ForgejoID: f.is.ID}}}
		id, err := r.s.store.SaveIssue(ctx, rec)
		if err != nil {
			return nil, err
		}
		rec.ID = id
		recs = append(recs, rec)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].CreatedAt.Before(recs[j].CreatedAt) })

	var kept []store.IssueRecord
	for _, rec := range recs {
		gone, err := r.issue(ctx, &rec)
		if err != nil {
			return nil, err
		}
		if !gone {
			kept = append(kept, rec)
		}
	}
	return kept, nil
}

// currentTitle is an issue's title as some node has it now.
func (r *run) currentTitle(rec store.IssueRecord) string {
	for _, n := range r.nodes {
		if c, ok := rec.Copies[n]; ok {
			if is, ok := r.snaps[n].issues[c.ForgejoID]; ok {
				return is.Title
			}
		}
	}
	return rec.BaseTitle
}

// issue brings one issue's copies together. gone reports that the record
// was deleted.
func (r *run) issue(ctx context.Context, rec *store.IssueRecord) (gone bool, err error) {
	ref := issueRef(*rec, r.primary)
	if rec.DeletedAt != nil {
		return r.deletedOnPrimary(ctx, rec, ref)
	}
	present := map[string]forgejo.Issue{}
	for _, n := range r.nodes {
		c, mapped := rec.Copies[n]
		if !mapped {
			continue
		}
		if is, ok := r.snaps[n].issues[c.ForgejoID]; ok {
			present[n] = is
		} else {
			// Deleted on that node.
			if n == r.primary {
				return r.deletedOnPrimary(ctx, rec, ref)
			}
			delete(rec.Copies, n)
		}
	}
	if len(present) == 0 {
		return false, nil
	}

	edit := func(title, body, state *string) func(context.Context, string, int64, string, forgejo.Issue) error {
		return func(ctx context.Context, n string, number int64, _ string, _ forgejo.Issue) error {
			return r.s.nodes[n].API.EditIssue(ctx, r.owner, r.name, number, title, body, state)
		}
	}
	fields := []struct {
		name string
		base *string
		get  func(node string, is forgejo.Issue) string
		set  func(ctx context.Context, node string, number int64, value string, is forgejo.Issue) error
		// skip leaves a node out of the comparison: what it has there isn't
		// anyone's decision, so it mustn't win. A skipped node is still
		// written with the agreed value instead, so it catches up.
		skip func(node string, is forgejo.Issue) bool
		// keep leaves a node alone altogether: not compared, not written.
		keep func(node string, is forgejo.Issue) bool
		// refuse says why a node couldn't hold a value at all. One that
		// can't makes the field a conflict for a person; nothing is written
		// and the base doesn't move, so the node that's behind agrees with
		// the base and never wins a later comparison.
		refuse func(node, value string) string
	}{
		{"title", &rec.BaseTitle, func(_ string, i forgejo.Issue) string { return i.Title }, nil, nil, nil, nil},
		{"body", &rec.BaseBody, func(_ string, i forgejo.Issue) string { return i.Body }, nil, nil, nil, nil},
		{"state", &rec.BaseState, func(_ string, i forgejo.Issue) string { return i.State }, nil, nil, nil, nil},
		{"labels", &rec.BaseLabels, r.labelsValue,
			func(ctx context.Context, n string, number int64, v string, is forgejo.Issue) error {
				fids, ok := r.labels.labelFIDs(n, v, &is, r.kept[n])
				if !ok {
					return errNotThereYet
				}
				return r.s.nodes[n].API.ReplaceIssueLabels(ctx, r.owner, r.name, number, fids)
			},
			func(n string, _ forgejo.Issue) bool { return r.restored[n] }, nil, nil},
		{"milestone", &rec.BaseMilestone, r.milestones.milestoneValue,
			func(ctx context.Context, n string, number int64, v string, _ forgejo.Issue) error {
				fid, ok := r.milestones.milestoneFID(n, v)
				if !ok {
					return errNotThereYet
				}
				return r.s.nodes[n].API.SetIssueMilestone(ctx, r.owner, r.name, number, fid)
			},
			// An issue holds one milestone, so a node keeping one for its
			// owner can't take part without losing it. Labels are a set, and
			// labelsValue and labelFIDs keep a held one there on their own.
			func(n string, _ forgejo.Issue) bool { return r.restored[n] },
			func(n string, is forgejo.Issue) bool { return r.kept[n][r.milestones.milestoneValue(n, is)] },
			nil},
		{"assignees", &rec.BaseAssignees, assigneesValue,
			func(ctx context.Context, n string, number int64, v string, _ forgejo.Issue) error {
				return r.s.nodes[n].API.SetIssueAssignees(ctx, r.owner, r.name, number, split(v))
			},
			nil, nil, r.refuseAssignees},
	}
	want := map[string]string{} // for new copies
	for _, f := range fields {
		vals := map[string]string{}
		skipped := map[string]bool{}
		for n, is := range present {
			if f.keep != nil && f.keep(n, is) {
				continue // the owner's to settle; leave the node as it is
			}
			if f.skip != nil && f.skip(n, is) {
				skipped[n] = true
				continue
			}
			vals[n] = f.get(n, is)
		}
		stuck := r.refused(f.refuse, *f.base) // already out of reach
		value, writes, conflict := merge(vals, *f.base)
		if conflict {
			r.conflict(ref+" "+f.name, map[string]any{"field": f.name, "values": clip(r.readable(f.name, vals)),
				"issue": numbers(*rec), "author": rec.Author})
			if is, ok := present[r.primary]; ok {
				want[f.name] = f.get(r.primary, is)
			} else {
				want[f.name] = *f.base
			}
			continue
		}
		if cant := append(stuck, r.refused(f.refuse, value)...); len(cant) > 0 {
			sort.Strings(cant)
			r.conflict(ref+" "+f.name, map[string]any{"field": f.name, "values": clip(vals),
				"issue": numbers(*rec), "author": rec.Author, "blocked": clipList(cant)})
			if is, ok := present[r.primary]; ok {
				want[f.name] = f.get(r.primary, is)
			} else {
				want[f.name] = *f.base
			}
			continue
		}
		want[f.name] = value
		// A node left out of the comparison still catches up: its label came
		// back, and putting it on the issue again is ForgeSync's own doing.
		for n := range skipped {
			if f.get(n, present[n]) != value {
				writes = append(writes, n)
			}
		}
		sort.Strings(writes)
		for _, n := range writes {
			set := f.set
			if set == nil {
				v := value
				switch f.name {
				case "title":
					set = edit(&v, nil, nil)
				case "body":
					set = edit(nil, &v, nil)
				case "state":
					set = edit(nil, nil, &v)
				}
			}
			if err := set(ctx, n, rec.Copies[n].Number, value, present[n]); err != nil {
				if !errors.Is(err, errNotThereYet) {
					r.s.log.Warn("issues: updating a copy failed", "repository", r.rec.FullName, "node", n, "issue", ref, "field", f.name, "error", err)
				}
				r.complete = false
				continue
			}
			r.s.log.Info("issue updated", "repository", r.rec.FullName, "node", n, "issue", rec.Copies[n].Number, "field", f.name)
		}
		*f.base = value
	}

	// Copies on nodes that don't have one yet, in creation order.
	var source string
	if _, ok := present[r.primary]; ok {
		source = r.primary
	} else {
		for n := range present {
			source = n
		}
	}
	for _, n := range r.nodes {
		if _, has := rec.Copies[n]; has {
			continue
		}
		if r.s.opts.EnsureUser != nil {
			if err := r.s.opts.EnsureUser(ctx, rec.Author, source, n); err != nil {
				r.s.log.Warn("issues: the author can't be created on the node; copy skipped", "repository", r.rec.FullName,
					"node", n, "issue", ref, "author", rec.Author, "error", err)
				r.complete = false
				continue
			}
		}
		// Only with every label and the milestone: a copy missing some would
		// look like someone removed them, and that would be copied everywhere.
		labelIDs, ok1 := r.labels.labelFIDs(n, want["labels"], nil, nil)
		milestone, ok2 := r.milestones.milestoneFID(n, want["milestone"])
		if !ok1 || !ok2 {
			r.s.log.Debug("issues: copy waits for its labels or milestone", "repository", r.rec.FullName, "node", n, "issue", ref)
			r.complete = false
			continue
		}
		// Assignees only where they'd be taken; elsewhere the copy is made
		// without them, and the node stays out of that comparison.
		var assignees []string
		if r.canAssign(n, want["assignees"]) {
			assignees = split(want["assignees"])
		} else {
			r.s.log.Info("issues: a copy is made without assignees the node won't take",
				"repository", r.rec.FullName, "node", n, "issue", ref, "assignees", want["assignees"])
			r.complete = false
		}
		is, err := r.s.nodes[n].As(rec.Author).CreateIssue(ctx, r.owner, r.name, want["title"], want["body"], want["state"] == "closed",
			labelIDs, milestone, assignees)
		if err != nil {
			r.s.log.Warn("issues: creating a copy failed", "repository", r.rec.FullName, "node", n, "issue", ref, "error", err)
			r.complete = false
			continue
		}
		rec.Copies[n] = store.IssueCopy{Number: is.Number, ForgejoID: is.ID}
		r.snaps[n].issues[is.ID], r.snaps[n].byNumber[is.Number] = is, is
		r.s.log.Info("issue copied", "repository", r.rec.FullName, "node", n, "issue", ref, "number", is.Number)
		// Record at once: a copy made but not recorded would be made twice.
		if _, err := r.s.store.SaveIssue(ctx, *rec); err != nil {
			return false, err
		}
	}
	// Reactions last, so a copy made in this run gets them too.
	if r.s.opts.Reactions {
		have := map[string]map[string]bool{}
		for n, c := range rec.Copies {
			have[n] = r.snaps[n].reactions[c.ForgejoID]
			if have[n] == nil {
				have[n] = map[string]bool{} // a copy just made
			}
		}
		rec.BaseReactions = r.syncReactions(ctx, ref, source, rec.BaseReactions, have,
			func(ctx context.Context, n, login, content string, add bool) error {
				api := r.s.nodes[n].As(login)
				if add {
					return api.AddIssueReaction(ctx, r.owner, r.name, rec.Copies[n].Number, content)
				}
				return api.RemoveIssueReaction(ctx, r.owner, r.name, rec.Copies[n].Number, content)
			})
	}
	if r.s.opts.Attachments {
		at := map[string]map[string]forgejo.Attachment{}
		for n, c := range rec.Copies {
			at[n] = r.snaps[n].attachments[c.ForgejoID]
			if at[n] == nil {
				at[n] = map[string]forgejo.Attachment{} // a copy just made
			}
		}
		rec.BaseAttachments = r.syncAttachments(ctx, ref, rec.Author, rec.BaseAttachments, at, attacher{
			upload: func(ctx context.Context, n, author, name string, content []byte) error {
				_, err := r.s.nodes[n].As(author).UploadIssueAttachment(ctx, r.owner, r.name, rec.Copies[n].Number, name, content)
				return err
			},
			remove: func(ctx context.Context, n string, a forgejo.Attachment) error {
				return r.s.nodes[n].API.DeleteIssueAttachment(ctx, r.owner, r.name, rec.Copies[n].Number, a.ID)
			},
		})
	}
	_, err = r.s.store.SaveIssue(ctx, *rec)
	return false, err
}

// deletedOnPrimary deletes the other copies of an issue deleted on the
// primary, where they're unchanged since the last sync. Changed copies stay,
// as conflicts, until someone deletes them too or recreates the issue on the
// primary; they're never copied back on their own.
func (r *run) deletedOnPrimary(ctx context.Context, rec *store.IssueRecord, ref string) (bool, error) {
	if rec.DeletedAt == nil {
		now := r.s.now().UTC()
		rec.DeletedAt = &now
	}
	delete(rec.Copies, r.primary)
	for _, n := range r.nodes {
		c, ok := rec.Copies[n]
		if !ok {
			continue
		}
		is, there := r.snaps[n].issues[c.ForgejoID]
		if !there {
			delete(rec.Copies, n)
			continue
		}
		if is.Title != rec.BaseTitle || is.Body != rec.BaseBody || is.State != rec.BaseState {
			r.conflict(ref+" deleted", map[string]any{"field": "deleted", "node": n, "issue": numbers(*rec), "author": rec.Author})
			continue
		}
		if err := r.s.nodes[n].API.DeleteIssue(ctx, r.owner, r.name, c.Number); err != nil {
			r.s.log.Warn("issues: deleting a copy failed", "repository", r.rec.FullName, "node", n, "issue", c.Number, "error", err)
			r.complete = false
			continue
		}
		r.s.log.Info("issue deleted as on the primary", "repository", r.rec.FullName, "node", n, "issue", c.Number)
		delete(rec.Copies, n)
	}
	if len(rec.Copies) == 0 {
		return true, r.s.store.DeleteIssueRecord(ctx, rec.ID)
	}
	_, err := r.s.store.SaveIssue(ctx, *rec)
	return false, err
}

func (r *run) comments(ctx context.Context, issues []store.IssueRecord, recs []store.CommentRecord) error {
	byID := map[string]*store.IssueRecord{}
	// node -> issue number there -> issue record
	byNumber := map[string]map[int64]*store.IssueRecord{}
	for i := range issues {
		is := &issues[i]
		if is.DeletedAt != nil {
			// Deleted on the primary, kept elsewhere as a conflict: its
			// comments there are left exactly as they are.
			continue
		}
		byID[is.ID] = is
		for n, c := range is.Copies {
			if byNumber[n] == nil {
				byNumber[n] = map[int64]*store.IssueRecord{}
			}
			byNumber[n][c.Number] = is
		}
	}
	known := map[string]map[int64]bool{}
	for _, n := range r.nodes {
		known[n] = map[int64]bool{}
	}
	var kept []store.CommentRecord
	for _, c := range recs {
		if byID[c.IssueID] == nil {
			continue // its issue went this run
		}
		for n, fid := range c.Copies {
			if known[n] != nil {
				known[n][fid] = true
			}
		}
		kept = append(kept, c)
	}
	recs = kept

	type fresh struct {
		node  string
		c     forgejo.IssueComment
		issue *store.IssueRecord
	}
	var news []fresh
	for _, n := range r.nodes {
		for _, c := range r.snaps[n].comments {
			if known[n][c.ID] {
				continue
			}
			if is := byNumber[n][c.IssueNumber()]; is != nil {
				news = append(news, fresh{n, c, is})
			}
		}
	}
	sort.Slice(news, func(i, j int) bool {
		if !news[i].c.Created.Equal(news[j].c.Created) {
			return news[i].c.Created.Before(news[j].c.Created)
		}
		return news[i].c.ID < news[j].c.ID
	})
	for _, f := range news {
		attached := false
		for i := range recs {
			c := &recs[i]
			if _, has := c.Copies[f.node]; has || c.IssueID != f.issue.ID || !strings.EqualFold(c.Author, f.c.User.Login) ||
				r.currentBody(*c) != f.c.Body {
				continue
			}
			c.Copies[f.node] = f.c.ID
			if f.node == r.primary {
				c.DeletedAt = nil // recreated on the primary to keep it
			}
			if _, err := r.s.store.SaveComment(ctx, *c); err != nil {
				return err
			}
			attached = true
			break
		}
		if attached {
			continue
		}
		c := store.CommentRecord{IssueID: f.issue.ID, OriginNode: f.node, Author: f.c.User.Login, CreatedAt: f.c.Created,
			BaseBody: f.c.Body, Copies: map[string]int64{f.node: f.c.ID}}
		id, err := r.s.store.SaveComment(ctx, c)
		if err != nil {
			return err
		}
		c.ID = id
		recs = append(recs, c)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].CreatedAt.Before(recs[j].CreatedAt) })
	for i := range recs {
		if err := r.comment(ctx, &recs[i], byID[recs[i].IssueID]); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) currentBody(c store.CommentRecord) string {
	for _, n := range r.nodes {
		if fid, ok := c.Copies[n]; ok {
			if cm, ok := r.snaps[n].comments[fid]; ok {
				return cm.Body
			}
		}
	}
	return c.BaseBody
}

func (r *run) comment(ctx context.Context, c *store.CommentRecord, issue *store.IssueRecord) error {
	ref := issueRef(*issue, r.primary) + " comment"
	present := map[string]forgejo.IssueComment{}
	deleted := c.DeletedAt != nil
	for _, n := range r.nodes {
		fid, mapped := c.Copies[n]
		if !mapped || deleted {
			continue
		}
		if cm, ok := r.snaps[n].comments[fid]; ok {
			present[n] = cm
			continue
		}
		if n != r.primary {
			delete(c.Copies, n) // deleted there: recreated below
			continue
		}
		deleted = true
	}
	if deleted {
		// Deleted on the primary: delete the unchanged copies elsewhere;
		// changed ones stay as conflicts and aren't copied back.
		if c.DeletedAt == nil {
			now := r.s.now().UTC()
			c.DeletedAt = &now
		}
		delete(c.Copies, r.primary)
		for _, m := range r.nodes {
			fid, ok := c.Copies[m]
			if !ok {
				continue
			}
			cm, there := r.snaps[m].comments[fid]
			if !there {
				delete(c.Copies, m)
				continue
			}
			if cm.Body != c.BaseBody {
				r.conflict(ref+" deleted", map[string]any{"field": "comment deleted", "node": m, "issue": numbers(*issue), "author": c.Author})
				continue
			}
			if err := r.s.nodes[m].API.DeleteIssueComment(ctx, r.owner, r.name, fid); err != nil {
				r.s.log.Warn("issues: deleting a comment copy failed", "repository", r.rec.FullName, "node", m, "error", err)
				r.complete = false
				continue
			}
			delete(c.Copies, m)
		}
		if len(c.Copies) == 0 {
			return r.s.store.DeleteCommentRecord(ctx, c.ID)
		}
		_, err := r.s.store.SaveComment(ctx, *c)
		return err
	}
	if len(present) == 0 {
		return nil
	}

	vals := map[string]string{}
	for n, cm := range present {
		vals[n] = cm.Body
	}
	body, writes, conflict := merge(vals, c.BaseBody)
	if conflict {
		r.conflict(ref, map[string]any{"field": "comment", "values": clip(vals), "issue": numbers(*issue), "author": c.Author})
		if cm, ok := present[r.primary]; ok {
			body = cm.Body
		} else {
			body = c.BaseBody
		}
	} else {
		for _, n := range writes {
			if err := r.s.nodes[n].API.EditIssueComment(ctx, r.owner, r.name, c.Copies[n], body); err != nil {
				r.s.log.Warn("issues: updating a comment copy failed", "repository", r.rec.FullName, "node", n, "error", err)
				r.complete = false
			}
		}
		c.BaseBody = body
	}

	source := r.primary
	if _, ok := present[source]; !ok {
		for n := range present {
			source = n
		}
	}
	for _, n := range r.nodes {
		if _, has := c.Copies[n]; has {
			continue
		}
		ic, ok := issue.Copies[n]
		if !ok {
			continue // the issue has no copy there yet
		}
		if r.s.opts.EnsureUser != nil {
			if err := r.s.opts.EnsureUser(ctx, c.Author, source, n); err != nil {
				r.s.log.Warn("issues: the comment's author can't be created on the node; copy skipped", "repository", r.rec.FullName,
					"node", n, "author", c.Author, "error", err)
				r.complete = false
				continue
			}
		}
		cm, err := r.s.nodes[n].As(c.Author).CreateIssueComment(ctx, r.owner, r.name, ic.Number, body)
		if err != nil {
			r.s.log.Warn("issues: copying a comment failed", "repository", r.rec.FullName, "node", n, "error", err)
			r.complete = false
			continue
		}
		c.Copies[n] = cm.ID
		if _, err := r.s.store.SaveComment(ctx, *c); err != nil {
			return err
		}
	}
	if r.s.opts.Reactions {
		have := map[string]map[string]bool{}
		for n, id := range c.Copies {
			have[n] = r.snaps[n].commentReactions[id]
			if have[n] == nil {
				have[n] = map[string]bool{} // a copy just made
			}
		}
		c.BaseReactions = r.syncReactions(ctx, ref, source, c.BaseReactions, have,
			func(ctx context.Context, n, login, content string, add bool) error {
				api := r.s.nodes[n].As(login)
				if add {
					return api.AddCommentReaction(ctx, r.owner, r.name, c.Copies[n], content)
				}
				return api.RemoveCommentReaction(ctx, r.owner, r.name, c.Copies[n], content)
			})
	}
	if r.s.opts.Attachments {
		at := map[string]map[string]forgejo.Attachment{}
		for n, id := range c.Copies {
			at[n] = r.snaps[n].commentAttachments[id]
			if at[n] == nil {
				at[n] = map[string]forgejo.Attachment{} // a copy just made
			}
		}
		c.BaseAttachments = r.syncAttachments(ctx, ref, c.Author, c.BaseAttachments, at, attacher{
			upload: func(ctx context.Context, n, author, name string, content []byte) error {
				_, err := r.s.nodes[n].As(author).UploadCommentAttachment(ctx, r.owner, r.name, c.Copies[n], name, content)
				return err
			},
			remove: func(ctx context.Context, n string, a forgejo.Attachment) error {
				return r.s.nodes[n].API.DeleteCommentAttachment(ctx, r.owner, r.name, c.Copies[n], a.ID)
			},
		})
	}
	_, err := r.s.store.SaveComment(ctx, *c)
	return err
}

// errNoSource: no node ForgeSync could read has the attachment any more.
var errNoSource = errors.New("no node has the file to copy")

// errNotThereYet: a label or milestone an issue should get has no copy on
// the node yet; the next run sets it.
var errNotThereYet = errors.New("label or milestone not on the node yet")

// readable turns item ids in labels and milestone values into names, for
// conflict details.
func (r *run) readable(field string, vals map[string]string) map[string]string {
	if field != "labels" && field != "milestone" {
		return vals
	}
	names := map[string]string{}
	for _, n := range r.nodes {
		for kind, items := range r.snaps[n].items {
			idx := r.labels
			key := "name"
			if kind == "milestone" {
				idx, key = r.milestones, "title"
			}
			for fid, f := range items {
				if id, ok := idx.toItem[n][fid]; ok {
					names[id] = f[key]
				}
			}
		}
	}
	out := map[string]string{}
	for n, v := range vals {
		var parts []string
		for _, id := range strings.Split(v, ",") {
			if id != "" {
				parts = append(parts, names[id])
			}
		}
		out[n] = strings.Join(parts, ", ")
	}
	return out
}

// numbers is an issue's number on each node, for conflict details.
func numbers(rec store.IssueRecord) map[string]int64 {
	out := map[string]int64{}
	for n, c := range rec.Copies {
		out[n] = c.Number
	}
	return out
}

// clipList keeps conflict details small.
func clipList(v []string) []string {
	if len(v) > 10 {
		return v[:10]
	}
	return v
}

// clip shortens values for conflict details.
func clip(vals map[string]string) map[string]string {
	out := map[string]string{}
	for n, v := range vals {
		if len(v) > 300 {
			v = v[:300] + "…"
		}
		out[n] = v
	}
	return out
}
