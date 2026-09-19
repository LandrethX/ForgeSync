package issues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/health"
	"scenegit.org/forgesync/internal/store"
)

// fakeNode is one node's alice/demo: issues and pull requests share the
// number sequence, as in Forgejo.
type fakeNode struct {
	mu         sync.Mutex
	name       string
	next       int64 // last number used
	nextID     int64
	issues     map[int64]*forgejo.Issue // by number
	comments   map[int64]*forgejo.IssueComment
	clock      time.Time
	writes     []string
	labels     map[int64]*forgejo.Label
	milestones map[int64]*forgejo.Milestone
	assignable map[string]bool // who the node would let an issue be assigned to
	// reactions are "<login>:<content>" sets, by issue number and comment id.
	reactions        map[int64]map[string]bool
	commentReactions map[int64]map[string]bool
	// refuses is a login the node won't let react, standing in for a person
	// ForgeSync can't create there.
	refuses string
	// files are the attachments by issue number and comment id, with their
	// content; nextFile numbers them. rejects is a file name the node won't
	// take, standing in for a type or size it doesn't allow.
	files       map[int64][]*fakeFile
	commentFile map[int64][]*fakeFile
	nextFile    int64
	rejects     string
	// pulls are the pull requests by number, and branches the branches the
	// node has; a copy is only opened once both of a pull request's are.
	pulls    map[int64]*forgejo.PullRequest
	branches map[string]bool
}

// fakeFile is one attachment on the node.
type fakeFile struct {
	id      int64
	name    string
	content []byte
}

func newFakeNode(name string) *fakeNode {
	return &fakeNode{name: name, issues: map[int64]*forgejo.Issue{}, comments: map[int64]*forgejo.IssueComment{},
		labels: map[int64]*forgejo.Label{}, milestones: map[int64]*forgejo.Milestone{},
		assignable:       map[string]bool{"alice": true, "bob": true, "carol": true},
		reactions:        map[int64]map[string]bool{},
		commentReactions: map[int64]map[string]bool{},
		files:            map[int64][]*fakeFile{},
		commentFile:      map[int64][]*fakeFile{},
		pulls:            map[int64]*forgejo.PullRequest{},
		branches:         map[string]bool{"main": true},
		nextID:           map[string]int64{"se": 1000, "dk": 2000, "de": 3000}[name], clock: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)}
}

func (f *fakeNode) tick() time.Time { f.clock = f.clock.Add(time.Second); return f.clock }

// skipNumber is a pull request taking a number in the shared sequence.
func (f *fakeNode) skipNumber() { f.mu.Lock(); f.next++; f.mu.Unlock() }

// open is a user opening an issue directly on the node.
func (f *fakeNode) open(author, title string) *forgejo.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.create(author, title, "", false)
}

func (f *fakeNode) create(author, title, body string, closed bool) *forgejo.Issue {
	f.next++
	f.nextID++
	st := "open"
	if closed {
		st = "closed"
	}
	is := &forgejo.Issue{ID: f.nextID, Number: f.next, Title: title, Body: body, State: st,
		User: forgejo.User{Login: author}, Created: f.tick()}
	f.issues[is.Number] = is
	return is
}

func (f *fakeNode) say(author string, number int64, body string) *forgejo.IssueComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addComment(author, number, body)
}

func (f *fakeNode) addComment(author string, number int64, body string) *forgejo.IssueComment {
	f.nextID++
	url := fmt.Sprintf("http://%s/api/v1/repos/alice/demo/issues/%d", f.name, number)
	c := &forgejo.IssueComment{ID: f.nextID, IssueURL: url,
		User: forgejo.User{Login: author}, Body: body, Created: f.tick()}
	// On a pull request Forgejo fills pull_request_url and leaves issue_url
	// empty, which is the only way to tell from the comment alone.
	if f.pulls[number] != nil {
		c.IssueURL, c.PRURL = "", fmt.Sprintf("http://%s/api/v1/repos/alice/demo/pulls/%d", f.name, number)
	}
	f.comments[c.ID] = c
	return c
}

func (f *fakeNode) issue(number int64) *forgejo.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[number]
}

func (f *fakeNode) byTitle(title string) *forgejo.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range f.issues {
		if is.Title == title {
			return is
		}
	}
	return nil
}

// commentIDs is the ids of an issue's comments on the node, in order.
func (f *fakeNode) commentIDs(number int64) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []int64
	for _, c := range f.comments {
		if c.Number() == number {
			ids = append(ids, c.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func (f *fakeNode) commentsOn(number int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var cs []*forgejo.IssueComment
	for _, c := range f.comments {
		if c.Number() == number {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	var out []string
	for _, c := range cs {
		out = append(out, c.User.Login+": "+c.Body)
	}
	return out
}

// fakeAPI is a node's API as one account.
type fakeAPI struct {
	n  *fakeNode
	as string
}

func (a fakeAPI) ListIssues(_ context.Context, _, _ string, page, limit int) ([]forgejo.Issue, error) {
	return a.listIssues(page, limit, false)
}

// ListIssuesAndPulls is Forgejo's type=all: the pull requests come too.
func (a fakeAPI) ListIssuesAndPulls(_ context.Context, _, _ string, page, limit int) ([]forgejo.Issue, error) {
	return a.listIssues(page, limit, true)
}

func (a fakeAPI) listIssues(page, limit int, withPulls bool) ([]forgejo.Issue, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	var all []forgejo.Issue
	for _, is := range a.n.issues {
		if is.PullRequest != nil && !withPulls {
			continue
		}
		all = append(all, *is)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Number > all[j].Number })
	start := (page - 1) * limit
	if start >= len(all) {
		return nil, nil
	}
	return all[start:min(start+limit, len(all))], nil
}

func (a fakeAPI) ListPulls(_ context.Context, _, _ string, page, _ int) ([]forgejo.PullRequest, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	if page > 1 {
		return nil, nil
	}
	var out []forgejo.PullRequest
	for _, pr := range a.n.pulls {
		out = append(out, *pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (a fakeAPI) Issue(_ context.Context, _, _ string, number int64) (forgejo.Issue, bool, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	is := a.n.issues[number]
	if is == nil {
		return forgejo.Issue{}, false, nil
	}
	return *is, true, nil
}

func (a fakeAPI) BranchExists(_ context.Context, _, _, branch string) (bool, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	return a.n.branches[branch], nil
}

func (a fakeAPI) CreatePullRequest(_ context.Context, _, _ string, opt forgejo.CreatePullRequestOption) (forgejo.PullRequest, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	is := a.n.create(a.as, opt.Title, opt.Body, false)
	is.PullRequest = &struct{}{}
	pr := &forgejo.PullRequest{Number: is.Number, Title: opt.Title, Body: opt.Body, State: "open",
		User: forgejo.User{Login: a.as}, Head: &forgejo.PRBranch{Ref: opt.Head}, Base: &forgejo.PRBranch{Ref: opt.Base}}
	a.n.pulls[is.Number] = pr
	a.n.writes = append(a.n.writes, "open pull request "+opt.Title+" as "+a.as)
	return *pr, nil
}

// openPull is someone opening a pull request on the node.
func (f *fakeNode) openPull(author, title, head, base string) *forgejo.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.create(author, title, "", false)
	is.PullRequest = &struct{}{}
	f.branches[head] = true
	f.pulls[is.Number] = &forgejo.PullRequest{Number: is.Number, Title: title, State: "open",
		User: forgejo.User{Login: author}, Head: &forgejo.PRBranch{Ref: head}, Base: &forgejo.PRBranch{Ref: base}}
	return is
}

// pullOn is a pull request's state on the node, or "" if it has none.
func (f *fakeNode) pullOn(number int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pr := f.pulls[number]; pr != nil {
		return pr.State
	}
	return ""
}

// mergePull is someone merging a pull request on the node.
func (f *fakeNode) mergePull(number int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[number].State, f.pulls[number].Merged = "closed", true
	f.issues[number].State = "closed"
}
func (a fakeAPI) ListRepoComments(_ context.Context, _, _ string, page, limit int) ([]forgejo.IssueComment, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	var all []forgejo.IssueComment
	for _, c := range a.n.comments {
		all = append(all, *c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	start := (page - 1) * limit
	if start >= len(all) {
		return nil, nil
	}
	return all[start:min(start+limit, len(all))], nil
}
func (a fakeAPI) CreateIssue(_ context.Context, _, _, title, body string, closed bool, labels []int64, milestone int64,
	assignees []string) (forgejo.Issue, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.writes = append(a.n.writes, "create "+title+" as "+a.as)
	is := a.n.create(a.as, title, body, closed)
	a.n.setLabels(is, labels)
	a.n.setMilestone(is, milestone)
	if err := a.n.assign(is, assignees); err != nil {
		return forgejo.Issue{}, err
	}
	return *is, nil
}

// assign is Forgejo replacing an issue's assignees: it refuses anyone
// without write access, after having removed the others.
func (f *fakeNode) assign(is *forgejo.Issue, logins []string) error {
	is.Assignees = nil
	for _, login := range logins {
		if !f.assignable[login] {
			return fmt.Errorf("%s does not have access to the repository", login)
		}
		is.Assignees = append(is.Assignees, forgejo.User{Login: login})
	}
	return nil
}

func (a fakeAPI) SetIssueAssignees(_ context.Context, _, _ string, number int64, logins []string) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.writes = append(a.n.writes, fmt.Sprintf("assignees #%d", number))
	return a.n.assign(a.n.issues[number], logins)
}

func (a fakeAPI) ListAssignees(_ context.Context, _, _ string) ([]forgejo.User, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	var out []forgejo.User
	for login, ok := range a.n.assignable {
		if ok {
			out = append(out, forgejo.User{Login: login})
		}
	}
	return out, nil
}

func (f *fakeNode) set(comment bool, id int64) map[string]bool {
	m := f.reactions
	if comment {
		m = f.commentReactions
	}
	if m[id] == nil {
		m[id] = map[string]bool{}
	}
	return m[id]
}

// react is a person reacting on the node.
func (f *fakeNode) react(number int64, login, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(false, number)[login+":"+content] = true
}

// unreact is that person taking it back.
func (f *fakeNode) unreact(number int64, login, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.set(false, number), login+":"+content)
}

// reactionsOn is an issue's reactions on the node, sorted.
func (f *fakeNode) reactionsOn(number int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return setValue(f.set(false, number))
}

// commentReactionsOn is a comment's, by its id on the node.
func (f *fakeNode) commentReactionsOn(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return setValue(f.set(true, id))
}

func (f *fakeNode) list(comment bool, id int64, page int) ([]forgejo.Reaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if page > 1 {
		return nil, nil
	}
	var out []forgejo.Reaction
	for e := range f.set(comment, id) {
		login, content := who(e)
		out = append(out, forgejo.Reaction{User: forgejo.User{Login: login}, Content: content})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User.Login < out[j].User.Login })
	return out, nil
}

func (a fakeAPI) change(comment bool, id int64, content string, add bool) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	if a.as == a.n.refuses {
		return fmt.Errorf("%s isn't a user on %s", a.as, a.n.name)
	}
	what := "reaction"
	if comment {
		what = "comment reaction"
	}
	a.n.writes = append(a.n.writes, fmt.Sprintf("%s %d", what, id))
	if add {
		a.n.set(comment, id)[a.as+":"+content] = true
	} else {
		delete(a.n.set(comment, id), a.as+":"+content)
	}
	return nil
}

func (a fakeAPI) IssueReactions(_ context.Context, _, _ string, number int64, page, _ int) ([]forgejo.Reaction, error) {
	return a.n.list(false, number, page)
}
func (a fakeAPI) CommentReactions(_ context.Context, _, _ string, id int64, page, _ int) ([]forgejo.Reaction, error) {
	return a.n.list(true, id, page)
}
func (a fakeAPI) AddIssueReaction(_ context.Context, _, _ string, number int64, content string) error {
	return a.change(false, number, content, true)
}
func (a fakeAPI) RemoveIssueReaction(_ context.Context, _, _ string, number int64, content string) error {
	return a.change(false, number, content, false)
}
func (a fakeAPI) AddCommentReaction(_ context.Context, _, _ string, id int64, content string) error {
	return a.change(true, id, content, true)
}
func (a fakeAPI) RemoveCommentReaction(_ context.Context, _, _ string, id int64, content string) error {
	return a.change(true, id, content, false)
}

func (f *fakeNode) fileList(comment bool, id int64) []*fakeFile {
	if comment {
		return f.commentFile[id]
	}
	return f.files[id]
}

func (f *fakeNode) putFile(comment bool, id int64, file *fakeFile) {
	if comment {
		f.commentFile[id] = append(f.commentFile[id], file)
		return
	}
	f.files[id] = append(f.files[id], file)
}

func (f *fakeNode) dropFile(comment bool, id, fileID int64) {
	list := f.fileList(comment, id)
	for i, x := range list {
		if x.id == fileID {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if comment {
		f.commentFile[id] = list
	} else {
		f.files[id] = list
	}
}

// attach is a person putting a file on an issue on the node.
func (f *fakeNode) attach(number int64, name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFile++
	f.putFile(false, number, &fakeFile{id: f.nextFile, name: name, content: []byte(content)})
}

// detach takes it off again.
func (f *fakeNode) detach(number int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.fileList(false, number) {
		if x.name == name {
			f.dropFile(false, number, x.id)
			return
		}
	}
}

// filesOn is an issue's attachments on the node as "<name>=<content>",
// sorted.
func (f *fakeNode) filesOn(number int64) string { return f.fileNames(false, number) }

// commentFilesOn is a comment's, by its id on the node.
func (f *fakeNode) commentFilesOn(id int64) string { return f.fileNames(true, id) }

func (f *fakeNode) fileNames(comment bool, id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, x := range f.fileList(comment, id) {
		out = append(out, x.name+"="+string(x.content))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func (a fakeAPI) attachments(comment bool, id int64) ([]forgejo.Attachment, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	var out []forgejo.Attachment
	for _, x := range a.n.fileList(comment, id) {
		out = append(out, forgejo.Attachment{ID: x.id, Name: x.name, Size: int64(len(x.content)),
			Type: "attachment", DownloadURL: fmt.Sprintf("http://%s/attachments/%d", a.n.name, x.id)})
	}
	return out, nil
}

func (a fakeAPI) IssueAttachments(_ context.Context, _, _ string, number int64) ([]forgejo.Attachment, error) {
	return a.attachments(false, number)
}
func (a fakeAPI) CommentAttachments(_ context.Context, _, _ string, id int64) ([]forgejo.Attachment, error) {
	return a.attachments(true, id)
}
func (a fakeAPI) upload(comment bool, id int64, name string, content []byte) (forgejo.Attachment, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	if name == a.n.rejects {
		return forgejo.Attachment{}, fmt.Errorf("%s won't take a file called %s", a.n.name, name)
	}
	a.n.nextFile++
	a.n.putFile(comment, id, &fakeFile{id: a.n.nextFile, name: name, content: content})
	a.n.writes = append(a.n.writes, "attach "+name)
	return forgejo.Attachment{ID: a.n.nextFile, Name: name, Size: int64(len(content)), Type: "attachment"}, nil
}
func (a fakeAPI) UploadIssueAttachment(_ context.Context, _, _ string, number int64, name string, content []byte) (forgejo.Attachment, error) {
	return a.upload(false, number, name, content)
}
func (a fakeAPI) UploadCommentAttachment(_ context.Context, _, _ string, id int64, name string, content []byte) (forgejo.Attachment, error) {
	return a.upload(true, id, name, content)
}
func (a fakeAPI) DeleteIssueAttachment(_ context.Context, _, _ string, number, id int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.dropFile(false, number, id)
	a.n.writes = append(a.n.writes, "detach")
	return nil
}
func (a fakeAPI) DeleteCommentAttachment(_ context.Context, _, _ string, commentID, id int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.dropFile(true, commentID, id)
	a.n.writes = append(a.n.writes, "detach")
	return nil
}

// Download serves the fake's own attachment URLs.
func (a fakeAPI) Download(_ context.Context, url string, max int64) ([]byte, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	id, err := strconv.ParseInt(url[strings.LastIndex(url, "/")+1:], 10, 64)
	if err != nil {
		return nil, err
	}
	for _, list := range [](map[int64][]*fakeFile){a.n.files, a.n.commentFile} {
		for _, files := range list {
			for _, x := range files {
				if x.id != id {
					continue
				}
				if int64(len(x.content)) > max {
					return nil, fmt.Errorf("larger than %d bytes", max)
				}
				return x.content, nil
			}
		}
	}
	return nil, fmt.Errorf("no attachment %d on %s", id, a.n.name)
}

// assignees is an issue's assignees on the node, sorted.
func (f *fakeNode) assignees(number int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, u := range f.issues[number].Assignees {
		out = append(out, u.Login)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// setAssignees is a person assigning an issue on the node.
func (f *fakeNode) setAssignees(number int64, logins ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.assign(f.issues[number], logins); err != nil {
		panic(err)
	}
}

func (f *fakeNode) setLabels(is *forgejo.Issue, ids []int64) {
	is.Labels = nil
	for _, id := range ids {
		if l := f.labels[id]; l != nil {
			is.Labels = append(is.Labels, *l)
			continue
		}
		// Not one of the repository's own labels (an organization's, say):
		// Forgejo attaches it all the same.
		is.Labels = append(is.Labels, forgejo.Label{ID: id, Name: fmt.Sprintf("other-%d", id)})
	}
}

func (f *fakeNode) setMilestone(is *forgejo.Issue, id int64) {
	is.Milestone = nil
	if id != 0 {
		is.Milestone = &struct {
			ID int64 `json:"id"`
		}{id}
	}
}

// label is a user adding a label on the node; it returns its id.
func (f *fakeNode) label(name, color string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.labels[f.nextID] = &forgejo.Label{ID: f.nextID, Name: name, Color: color}
	return f.nextID
}

func (f *fakeNode) labelNamed(name string) *forgejo.Label {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.labels {
		if l.Name == name {
			return l
		}
	}
	return nil
}

func (f *fakeNode) milestone(title string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.milestones[f.nextID] = &forgejo.Milestone{ID: f.nextID, Title: title, State: "open"}
	return f.nextID
}

func (f *fakeNode) milestoneNamed(title string) *forgejo.Milestone {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.milestones {
		if m.Title == title {
			return m
		}
	}
	return nil
}

// labelNames is an issue's label names on the node, sorted.
func (f *fakeNode) labelNames(number int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.issues[number].Labels {
		if own := f.labels[l.ID]; own != nil { // the repository's own labels
			out = append(out, own.Name)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (f *fakeNode) milestoneTitle(number int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.issues[number].Milestone; m != nil {
		return f.milestones[m.ID].Title
	}
	return ""
}

func (a fakeAPI) SetIssueMilestone(_ context.Context, _, _ string, number, milestone int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.setMilestone(a.n.issues[number], milestone)
	a.n.writes = append(a.n.writes, fmt.Sprintf("milestone #%d", number))
	return nil
}
func (a fakeAPI) ReplaceIssueLabels(_ context.Context, _, _ string, number int64, labels []int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.setLabels(a.n.issues[number], labels)
	a.n.writes = append(a.n.writes, fmt.Sprintf("labels #%d", number))
	return nil
}
func (a fakeAPI) ListLabels(_ context.Context, _, _ string, page, _ int) ([]forgejo.Label, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	if page > 1 {
		return nil, nil
	}
	var out []forgejo.Label
	for _, l := range a.n.labels {
		out = append(out, *l)
	}
	return out, nil
}
func (a fakeAPI) CreateLabel(_ context.Context, _, _ string, l forgejo.Label) (forgejo.Label, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.nextID++
	l.ID = a.n.nextID
	l.Color = strings.TrimPrefix(l.Color, "#")
	a.n.labels[l.ID] = &l
	a.n.writes = append(a.n.writes, "create label "+l.Name)
	return l, nil
}
func (a fakeAPI) EditLabel(_ context.Context, _, _ string, id int64, fields map[string]any) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	l := a.n.labels[id]
	for k, v := range fields {
		switch k {
		case "name":
			l.Name = v.(string)
		case "color":
			l.Color = strings.TrimPrefix(v.(string), "#")
		case "description":
			l.Description = v.(string)
		}
	}
	a.n.writes = append(a.n.writes, "edit label")
	return nil
}
func (a fakeAPI) DeleteLabel(_ context.Context, _, _ string, id int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	delete(a.n.labels, id)
	for _, is := range a.n.issues { // Forgejo drops it from issues too
		var keep []forgejo.Label
		for _, l := range is.Labels {
			if l.ID != id {
				keep = append(keep, l)
			}
		}
		is.Labels = keep
	}
	a.n.writes = append(a.n.writes, "delete label")
	return nil
}
func (a fakeAPI) ListMilestones(_ context.Context, _, _ string, page, _ int) ([]forgejo.Milestone, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	if page > 1 {
		return nil, nil
	}
	var out []forgejo.Milestone
	for _, m := range a.n.milestones {
		out = append(out, *m)
	}
	return out, nil
}
func (a fakeAPI) CreateMilestone(_ context.Context, _, _ string, m forgejo.Milestone) (forgejo.Milestone, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.nextID++
	m.ID = a.n.nextID
	a.n.milestones[m.ID] = &m
	a.n.writes = append(a.n.writes, "create milestone "+m.Title)
	return m, nil
}
func (a fakeAPI) EditMilestone(_ context.Context, _, _ string, id int64, fields map[string]any) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	m := a.n.milestones[id]
	for k, v := range fields {
		switch k {
		case "title":
			m.Title = v.(string)
		case "state":
			m.State = v.(string)
		case "due_on":
			t := v.(time.Time)
			m.Deadline = &t
		}
	}
	a.n.writes = append(a.n.writes, "edit milestone")
	return nil
}
func (a fakeAPI) DeleteMilestone(_ context.Context, _, _ string, id int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	delete(a.n.milestones, id)
	for _, is := range a.n.issues {
		if is.Milestone != nil && is.Milestone.ID == id {
			is.Milestone = nil
		}
	}
	a.n.writes = append(a.n.writes, "delete milestone")
	return nil
}
func (a fakeAPI) EditIssue(_ context.Context, _, _ string, number int64, title, body, state *string) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	is := a.n.issues[number]
	if is == nil {
		return errors.New("404")
	}
	if title != nil {
		is.Title = *title
		a.n.writes = append(a.n.writes, fmt.Sprintf("title #%d", number))
	}
	if body != nil {
		is.Body = *body
		a.n.writes = append(a.n.writes, fmt.Sprintf("body #%d", number))
	}
	if state != nil {
		is.State = *state
		// Closing a pull request through the issue endpoint closes the pull
		// request; it never merges it.
		if pr := a.n.pulls[number]; pr != nil {
			pr.State = *state
		}
		a.n.writes = append(a.n.writes, fmt.Sprintf("state #%d", number))
	}
	return nil
}
func (a fakeAPI) DeleteIssue(_ context.Context, _, _ string, number int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	delete(a.n.issues, number)
	a.n.writes = append(a.n.writes, fmt.Sprintf("delete #%d", number))
	return nil
}
func (a fakeAPI) CreateIssueComment(_ context.Context, _, _ string, number int64, body string) (forgejo.IssueComment, error) {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.writes = append(a.n.writes, fmt.Sprintf("comment #%d as %s", number, a.as))
	return *a.n.addComment(a.as, number, body), nil
}
func (a fakeAPI) EditIssueComment(_ context.Context, _, _ string, id int64, body string) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	a.n.comments[id].Body = body
	a.n.writes = append(a.n.writes, "edit comment")
	return nil
}
func (a fakeAPI) DeleteIssueComment(_ context.Context, _, _ string, id int64) error {
	a.n.mu.Lock()
	defer a.n.mu.Unlock()
	delete(a.n.comments, id)
	a.n.writes = append(a.n.writes, "delete comment")
	return nil
}

// memStore keeps records in memory.
type memStore struct {
	mu       sync.Mutex
	rec      store.RepositoryRecord
	issues   map[string]store.IssueRecord
	comments map[string]store.CommentRecord
	seq      int
	found    []store.FoundConflict
	checked  []string
	items    map[string]store.RepoItem
}

func (m *memStore) RepoItems(context.Context, string) ([]store.RepoItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.RepoItem
	for _, it := range m.items {
		cp := map[string]int64{}
		for k, v := range it.Copies {
			cp[k] = v
		}
		base := map[string]string{}
		for k, v := range it.Base {
			base[k] = v
		}
		it.Copies, it.Base = cp, base
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *memStore) SaveRepoItem(_ context.Context, it store.RepoItem) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it.ID == "" {
		m.seq++
		it.ID = fmt.Sprintf("x%03d", m.seq)
	}
	cp := map[string]int64{}
	for k, v := range it.Copies {
		cp[k] = v
	}
	it.Copies = cp
	m.items[it.ID] = it
	return it.ID, nil
}
func (m *memStore) DeleteRepoItem(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

func newMemStore() *memStore {
	return &memStore{rec: store.RepositoryRecord{ID: "11111111-1111-1111-1111-111111111111", FullName: "alice/demo", PrimaryNode: "se"},
		issues: map[string]store.IssueRecord{}, comments: map[string]store.CommentRecord{}, items: map[string]store.RepoItem{}}
}

func (m *memStore) Repositories(context.Context) ([]store.RepositoryRecord, error) {
	return []store.RepositoryRecord{m.rec}, nil
}
func (m *memStore) Repository(context.Context, string) (store.RepositoryRecord, error) {
	return m.rec, nil
}
func (m *memStore) Issues(context.Context, string) ([]store.IssueRecord, []store.CommentRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var is []store.IssueRecord
	for _, r := range m.issues {
		r.Copies = cloneCopies(r.Copies)
		is = append(is, r)
	}
	sort.Slice(is, func(i, j int) bool { return is[i].CreatedAt.Before(is[j].CreatedAt) })
	var cs []store.CommentRecord
	for _, c := range m.comments {
		cp := map[string]int64{}
		for k, v := range c.Copies {
			cp[k] = v
		}
		c.Copies = cp
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].CreatedAt.Before(cs[j].CreatedAt) })
	return is, cs, nil
}
func cloneCopies(in map[string]store.IssueCopy) map[string]store.IssueCopy {
	out := map[string]store.IssueCopy{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func (m *memStore) SaveIssue(_ context.Context, r store.IssueRecord) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == "" {
		m.seq++
		r.ID = fmt.Sprintf("i%d", m.seq)
	}
	r.Copies = cloneCopies(r.Copies)
	m.issues[r.ID] = r
	return r.ID, nil
}
func (m *memStore) DeleteIssueRecord(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.issues, id)
	for k, c := range m.comments {
		if c.IssueID == id {
			delete(m.comments, k)
		}
	}
	return nil
}
func (m *memStore) SaveComment(_ context.Context, c store.CommentRecord) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		m.seq++
		c.ID = fmt.Sprintf("c%d", m.seq)
	}
	cp := map[string]int64{}
	for k, v := range c.Copies {
		cp[k] = v
	}
	c.Copies = cp
	m.comments[c.ID] = c
	return c.ID, nil
}
func (m *memStore) DeleteCommentRecord(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.comments, id)
	return nil
}
func (m *memStore) SyncConflicts(_ context.Context, found []store.FoundConflict, checked, _ []string, _ time.Time) ([]store.ConflictChange, error) {
	m.found, m.checked = found, checked
	return nil, nil
}
func (m *memStore) Audit(context.Context, string, string, string, map[string]any) error { return nil }

type allHealthy []string

func (a allHealthy) Snapshot() []health.Status {
	var out []health.Status
	for _, n := range a {
		out = append(out, health.Status{Node: n, State: health.Healthy})
	}
	return out
}

func setup(t *testing.T, names ...string) (*Syncer, *memStore, map[string]*fakeNode, *[]string) {
	t.Helper()
	if len(names) == 0 {
		names = []string{"se", "dk", "de"}
	}
	st := newMemStore()
	fakes := map[string]*fakeNode{}
	var nodes []Node
	var ensured []string
	for _, n := range names {
		f := newFakeNode(n)
		fakes[n] = f
		nodes = append(nodes, Node{Name: n, API: fakeAPI{f, "forgesync"}, As: func(login string) API { return fakeAPI{f, login} }})
	}
	s := NewSyncer(nodes, st, allHealthy(names), Options{EnsureUser: func(_ context.Context, login, from, to string) error {
		if login == "ghost" {
			return errors.New("not a SceneID user")
		}
		ensured = append(ensured, login+"@"+to)
		return nil
	}}, slog.New(slog.DiscardHandler))
	return s, st, fakes, &ensured
}

func (s *Syncer) run(t *testing.T) {
	t.Helper()
	if err := s.RunRepo(context.Background(), "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatal(err)
	}
}

func writes(fakes map[string]*fakeNode) int {
	n := 0
	for _, f := range fakes {
		f.mu.Lock()
		n += len(f.writes)
		f.writes = nil
		f.mu.Unlock()
	}
	return n
}

func TestNewIssuesAreCopiedAsTheirAuthor(t *testing.T) {
	s, st, f, ensured := setup(t)
	f["se"].open("alice", "on se")
	f["dk"].open("bob", "on dk") // same number 1 on dk, made later
	s.run(t)

	for _, n := range []string{"se", "dk", "de"} {
		se, dk := f[n].byTitle("on se"), f[n].byTitle("on dk")
		if se == nil || dk == nil || se.User.Login != "alice" || dk.User.Login != "bob" {
			t.Fatalf("%s: %+v / %+v", n, se, dk)
		}
	}
	// On de, created in creation order: numbers match the origin where possible.
	if f["de"].byTitle("on se").Number != 1 || f["de"].byTitle("on dk").Number != 2 {
		t.Errorf("de numbers: %d, %d", f["de"].byTitle("on se").Number, f["de"].byTitle("on dk").Number)
	}
	if len(st.issues) != 2 || len(st.checked) != 1 || len(st.found) != 0 {
		t.Errorf("records %d, checked %v, conflicts %v", len(st.issues), st.checked, st.found)
	}
	if !strings.Contains(strings.Join(*ensured, ","), "bob@se") {
		t.Errorf("bob wasn't created on se first: %v", *ensured)
	}
	writes(f)
	s.run(t)
	if n := writes(f); n != 0 {
		t.Errorf("a second run wrote %d times", n)
	}
}

func TestEditsAreMergedPerField(t *testing.T) {
	s, st, f, _ := setup(t)
	f["se"].open("alice", "first")
	s.run(t)
	writes(f)

	// Title changed on a replica, body on the primary, closed on another
	// replica: all three carry over everywhere.
	f["dk"].issue(1).Title = "better title"
	f["se"].issue(1).Body = "details"
	f["de"].issue(1).State = "closed"
	s.run(t)
	for _, n := range []string{"se", "dk", "de"} {
		is := f[n].issue(1)
		if is.Title != "better title" || is.Body != "details" || is.State != "closed" {
			t.Errorf("%s: %+v", n, is)
		}
	}
	if len(st.found) != 0 {
		t.Errorf("conflicts: %+v", st.found)
	}

	// Different titles on two nodes: a conflict, nothing overwritten.
	writes(f)
	f["se"].issue(1).Title = "title A"
	f["dk"].issue(1).Title = "title B"
	s.run(t)
	if len(st.found) != 1 || st.found[0].Ref != "#1 title" || f["de"].issue(1).Title != "better title" || writes(f) != 0 {
		t.Fatalf("conflict = %+v, de title %q", st.found, f["de"].issue(1).Title)
	}
	// Someone settles it by making one side match: it goes everywhere.
	f["dk"].issue(1).Title = "title A"
	s.run(t)
	if len(st.found) != 0 || f["de"].issue(1).Title != "title A" {
		t.Errorf("after settling: %+v, de %q", st.found, f["de"].issue(1).Title)
	}
}

func TestNumbersCanDiffer(t *testing.T) {
	s, st, f, _ := setup(t, "se", "dk")
	f["dk"].skipNumber() // a pull request took #1 on dk only
	f["se"].open("alice", "first")
	s.run(t)
	var rec store.IssueRecord
	for _, r := range st.issues {
		rec = r
	}
	if rec.Copies["se"].Number != 1 || rec.Copies["dk"].Number != 2 {
		t.Fatalf("copies = %+v", rec.Copies)
	}
	// Comments still land on the right issue on each node.
	f["dk"].say("carol", 2, "seen on dk")
	s.run(t)
	if got := f["se"].commentsOn(1); len(got) != 1 || got[0] != "carol: seen on dk" {
		t.Errorf("se #1 comments = %v", got)
	}
}

func TestComments(t *testing.T) {
	s, st, f, _ := setup(t)
	f["se"].open("alice", "first")
	s.run(t)
	c := f["dk"].say("bob", 1, "hello")
	s.run(t)
	for _, n := range []string{"se", "de"} {
		if got := f[n].commentsOn(1); len(got) != 1 || got[0] != "bob: hello" {
			t.Fatalf("%s comments = %v", n, got)
		}
	}
	// Edited on the replica: everywhere.
	c.Body = "hello, edited"
	s.run(t)
	if got := f["de"].commentsOn(1); got[0] != "bob: hello, edited" {
		t.Errorf("de = %v", got)
	}
	// Deleted on a replica: recreated. Deleted on the primary: gone everywhere.
	f["de"].mu.Lock()
	for id := range f["de"].comments {
		delete(f["de"].comments, id)
	}
	f["de"].mu.Unlock()
	s.run(t)
	if got := f["de"].commentsOn(1); len(got) != 1 {
		t.Fatalf("not recreated on de: %v", got)
	}
	f["se"].mu.Lock()
	for id := range f["se"].comments {
		delete(f["se"].comments, id)
	}
	f["se"].mu.Unlock()
	s.run(t)
	if len(f["dk"].commentsOn(1))+len(f["de"].commentsOn(1)) != 0 || len(st.comments) != 0 {
		t.Errorf("after deleting on the primary: dk %v de %v records %d", f["dk"].commentsOn(1), f["de"].commentsOn(1), len(st.comments))
	}
}

func TestIssueDeletedOnPrimary(t *testing.T) {
	s, st, f, _ := setup(t)
	f["se"].open("alice", "keep")
	f["se"].open("alice", "delete me")
	f["se"].open("alice", "changed elsewhere")
	s.run(t)
	f["dk"].issue(3).Body = "dk's own change"
	f["se"].mu.Lock()
	delete(f["se"].issues, 2)
	delete(f["se"].issues, 3)
	f["se"].mu.Unlock()
	s.run(t)
	if f["dk"].issue(2) != nil || f["de"].issue(2) != nil {
		t.Error("the unchanged copies of #2 weren't deleted")
	}
	if f["dk"].issue(3) == nil || f["de"].issue(3) != nil {
		t.Error("#3: dk's changed copy must stay, de's unchanged one go")
	}
	if len(st.found) != 1 || st.found[0].Ref != "#3 deleted" {
		t.Errorf("conflicts = %+v", st.found)
	}
	// Later runs don't bring #3 back to the primary or touch dk's copy and
	// its comment.
	f["dk"].say("bob", 3, "still relevant")
	s.run(t)
	s.run(t)
	if f["se"].byTitle("changed elsewhere") != nil || f["dk"].issue(3) == nil || len(f["dk"].commentsOn(3)) != 1 || len(st.found) != 1 {
		t.Fatalf("after more runs: se %+v, dk %+v %v, conflicts %+v",
			f["se"].byTitle("changed elsewhere"), f["dk"].issue(3), f["dk"].commentsOn(3), st.found)
	}
	// Recreating it on the primary keeps it: normal again.
	f["se"].open("alice", "changed elsewhere")
	s.run(t)
	if len(st.found) != 0 || f["de"].byTitle("changed elsewhere") == nil {
		t.Errorf("after recreating on the primary: conflicts %+v, de %+v", st.found, f["de"].byTitle("changed elsewhere"))
	}
	// Deleted on a replica: recreated there.
	f["de"].mu.Lock()
	delete(f["de"].issues, 1)
	f["de"].mu.Unlock()
	s.run(t)
	if f["de"].byTitle("keep") == nil {
		t.Error("#1 not recreated on de")
	}
}

func TestExistingIdenticalIssuesAreAdopted(t *testing.T) {
	s, st, f, _ := setup(t, "se", "dk")
	f["se"].open("alice", "already here")
	f["dk"].open("alice", "already here")
	s.run(t)
	if len(st.issues) != 1 || len(f["se"].issues) != 1 || len(f["dk"].issues) != 1 {
		t.Errorf("records %d, se %d, dk %d", len(st.issues), len(f["se"].issues), len(f["dk"].issues))
	}
}

func TestAuthorThatCantBeCreated(t *testing.T) {
	s, st, f, _ := setup(t, "se", "dk")
	f["se"].open("ghost", "by a deleted user")
	s.run(t)
	if len(f["dk"].issues) != 0 || len(st.checked) != 0 {
		t.Errorf("dk issues %d, checked %v", len(f["dk"].issues), st.checked)
	}
}

func TestCommentDeletedOnPrimaryButEditedElsewhere(t *testing.T) {
	s, st, f, _ := setup(t, "se", "dk")
	f["se"].open("alice", "first")
	f["se"].say("alice", 1, "original")
	s.run(t)
	for _, c := range f["dk"].comments {
		c.Body = "edited on dk"
	}
	f["se"].mu.Lock()
	for id := range f["se"].comments {
		delete(f["se"].comments, id)
	}
	f["se"].mu.Unlock()
	s.run(t)
	s.run(t)
	if got := f["dk"].commentsOn(1); len(got) != 1 || got[0] != "alice: edited on dk" {
		t.Errorf("dk comments = %v", got)
	}
	if len(f["se"].commentsOn(1)) != 0 || len(st.found) != 1 || !strings.HasSuffix(st.found[0].Ref, "comment deleted") {
		t.Errorf("se %v, conflicts %+v", f["se"].commentsOn(1), st.found)
	}
}
