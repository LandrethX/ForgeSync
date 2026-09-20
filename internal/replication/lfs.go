package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"scenegit.org/forgesync/internal/store"
)

// Git LFS keeps large files out of the repository: what git carries is a
// pointer file naming a sha256 and a size, and the bytes themselves live in
// the node's LFS store, fetched over the LFS protocol. Replicating the refs
// alone therefore gives a replica a repository it can't check out: the
// pointers are there and the files they name are not.
//
// So the objects travel too, and they're the one thing in ForgeSync that
// can't be in conflict: an object's name *is* the hash of its content, so
// two nodes can never mean different things by one oid, and an object is
// never edited, only referenced by more commits. That makes the rule
// simple -- every node that has the repository should have every object any
// node has -- and it makes it safe: ForgeSync only ever adds. It never
// deletes an LFS object, even one nothing references any more, because that
// is a garbage collection decision for the node that holds it, and because
// an object no ref reaches today may be reached again by a commit that
// arrives tomorrow.
//
// The objects are found the way git finds them: every blob small enough to
// be a pointer file, under any ref, is read and parsed (`Git.Pointers`).
// Whether a node has one is asked with the LFS batch API itself -- an
// upload request for an object the server already has comes back with
// nothing to do -- so ForgeSync never keeps a list that could go stale.

// LFSConflictKind is the conflict kind this pass owns: a node the objects
// couldn't be copied to.
const LFSConflictKind = "lfs_incomplete"

// lfsBatchSize is how many objects one batch request asks about. Forgejo
// takes more, but a bounded request keeps one unreachable node from timing
// out a whole repository's worth of objects at once.
const lfsBatchSize = 100

// syncLFS gives every node that has the repository every LFS object any of
// them has. The cache already holds the primary's refs; the pointers come
// from there.
func (e *Engine) syncLFS(ctx context.Context, dir string, rec store.RepositoryRecord, healthy map[string]bool) {
	var nodes []Node
	for _, n := range e.order {
		if healthy[n] && e.hasRepo(rec, n) {
			nodes = append(nodes, e.nodes[n])
		}
	}
	if len(nodes) < 2 {
		return
	}
	ptrs, err := e.git.Pointers(ctx, dir, []string{"refs/forgesync/primary/heads/*", "refs/forgesync/primary/tags/*"})
	if err != nil {
		e.log.Warn("lfs: looking for pointer files failed", "repository", rec.FullName, "error", err)
		return
	}
	if len(ptrs) == 0 {
		// Nothing uses LFS here; clear anything an earlier run reported.
		e.reportLFS(ctx, rec, nil)
		return
	}

	// Who has what. An upload request for an object the node already has
	// comes back without an upload action, which is the question asked.
	have := map[string]map[string]bool{} // node -> oid -> it has it
	failed := map[string]string{}        // node -> why it couldn't be asked
	for _, n := range nodes {
		lack, err := e.lfs(n, rec).missing(ctx, ptrs)
		if err != nil {
			e.log.Warn("lfs: asking a node what it has failed", "repository", rec.FullName, "node", n.Name, "error", err)
			failed[n.Name] = err.Error()
			continue
		}
		on := map[string]bool{}
		for _, p := range ptrs {
			on[p.OID] = true
		}
		for _, p := range lack {
			on[p.OID] = false
		}
		have[n.Name] = on
	}
	for _, n := range nodes {
		for _, p := range ptrs {
			if on, asked := have[n.Name]; !asked || on[p.OID] {
				continue
			}
			from, ok := lfsSource(nodes, have, p)
			if !ok {
				// Nobody has it: the pointer is in git but the file never
				// reached any node. That's the repository's own problem,
				// not a difference between nodes, so it isn't reported as
				// one.
				continue
			}
			if err := e.carryLFS(ctx, rec, p, from, n); err != nil {
				e.log.Warn("lfs: copying an object failed; left for the next run", "repository", rec.FullName,
					"oid", p.OID, "from", from.Name, "to", n.Name, "error", err)
				continue
			}
			have[n.Name][p.OID] = true
			e.log.Info("lfs object copied", "repository", rec.FullName, "oid", p.OID, "size", p.Size,
				"from", from.Name, "to", n.Name)
			e.audit(ctx, "repo.lfs_object_copied", rec.FullName,
				map[string]any{"repository_id": rec.ID, "oid": p.OID, "size": p.Size, "from": from.Name, "node": n.Name})
		}
	}
	e.reportLFS(ctx, rec, lfsTrouble(e.order, rec, ptrs, have, failed))
}

// carryLFS moves one object between nodes through a file on disk, so a
// large one costs disk rather than memory.
func (e *Engine) carryLFS(ctx context.Context, rec store.RepositoryRecord, p Pointer, from, to Node) error {
	if e.opts.LFSMax > 0 && p.Size > e.opts.LFSMax {
		return fmt.Errorf("the object is %d bytes, over replication.lfs_max_bytes (%d)", p.Size, e.opts.LFSMax)
	}
	tmp, err := os.CreateTemp(e.git.WorkDir, "lfs-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if err := e.lfs(from, rec).download(ctx, p, tmp); err != nil {
		return fmt.Errorf("from %s: %w", from.Name, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := e.lfs(to, rec).upload(ctx, p, tmp); err != nil {
		return fmt.Errorf("to %s: %w", to.Name, err)
	}
	return nil
}

// lfsSource picks a node that has the object. e.order puts the nodes in
// their configured order, and the caller passes them that way, so this is
// stable rather than whichever map iteration came first.
func lfsSource(nodes []Node, have map[string]map[string]bool, p Pointer) (Node, bool) {
	for _, n := range nodes {
		if have[n.Name][p.OID] {
			return n, true
		}
	}
	return Node{}, false
}

// lfsTrouble describes the nodes still short of objects after a run: the
// ones that couldn't be asked, and the ones an object another node has
// couldn't be copied to. An object no node has isn't counted -- nothing on
// any node differs, and ForgeSync has nothing to copy.
func lfsTrouble(order []string, rec store.RepositoryRecord, ptrs []Pointer,
	have map[string]map[string]bool, failed map[string]string) []store.FoundConflict {
	var found []store.FoundConflict
	for _, n := range order {
		if failed[n] != "" {
			found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: LFSConflictKind, Ref: n,
				Details: map[string]any{"node": n, "error": failed[n]}})
			continue
		}
		on, asked := have[n]
		if !asked {
			continue
		}
		var oids []string
		for _, p := range ptrs {
			if !on[p.OID] && anyHas(have, p) {
				oids = append(oids, p.OID)
			}
		}
		if len(oids) == 0 {
			continue
		}
		sort.Strings(oids)
		shown := oids
		if len(shown) > 10 {
			shown = shown[:10]
		}
		found = append(found, store.FoundConflict{RepositoryID: rec.ID, Kind: LFSConflictKind, Ref: n,
			Details: map[string]any{"node": n, "objects": len(oids), "oids": shown}})
	}
	return found
}

func anyHas(have map[string]map[string]bool, p Pointer) bool {
	for _, on := range have {
		if on[p.OID] {
			return true
		}
	}
	return false
}

func (e *Engine) reportLFS(ctx context.Context, rec store.RepositoryRecord, found []store.FoundConflict) {
	if _, err := e.store.SyncConflicts(ctx, found, []string{rec.ID}, []string{LFSConflictKind}, e.now()); err != nil {
		e.log.Error("lfs: recording the conflicts failed", "repository", rec.FullName, "error", err)
	}
}

func (e *Engine) lfs(n Node, rec store.RepositoryRecord) *lfsClient {
	name := rec.FullName
	for _, rp := range rec.Replicas {
		if rp.Node == n.Name && rp.FullName != "" {
			name = rp.FullName // its actual name on that node
		}
	}
	return &lfsClient{remote: e.remote(n, name), http: e.httpClient()}
}

func (e *Engine) httpClient() *http.Client {
	if e.http != nil {
		return e.http
	}
	return http.DefaultClient
}

// lfsClient speaks the Git LFS batch API of one repository on one node.
// It's the protocol, not Forgejo's REST API: the same requests git-lfs
// itself makes, with the service account's token as basic auth.
type lfsClient struct {
	remote Remote
	http   *http.Client
}

type lfsObject struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsError            `json:"error,omitempty"`
}

type lfsAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header,omitempty"`
}

type lfsError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error is the status and message the LFS endpoint answered with.
func (e *lfsError) Error() string { return fmt.Sprintf("%d %s", e.Code, e.Message) }

const lfsMediaType = "application/vnd.git-lfs+json"

// endpoint is the LFS endpoint git-lfs derives from a repository's URL.
func (c *lfsClient) endpoint(path string) string {
	return strings.TrimSuffix(c.remote.URL, "/") + "/info/lfs" + path
}

// missing asks which of the objects the node hasn't got, in batches. An
// upload request is the question: the server answers with an upload action
// only for what it lacks.
func (c *lfsClient) missing(ctx context.Context, ptrs []Pointer) ([]Pointer, error) {
	var out []Pointer
	for start := 0; start < len(ptrs); start += lfsBatchSize {
		end := min(start+lfsBatchSize, len(ptrs))
		objs, err := c.batch(ctx, "upload", ptrs[start:end])
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if o.Error != nil {
				return nil, fmt.Errorf("object %s: %w", o.OID, o.Error)
			}
			if _, ok := o.Actions["upload"]; ok {
				out = append(out, Pointer{OID: o.OID, Size: o.Size})
			}
		}
	}
	return out, nil
}

func (c *lfsClient) batch(ctx context.Context, operation string, ptrs []Pointer) ([]lfsObject, error) {
	objs := make([]lfsObject, 0, len(ptrs))
	for _, p := range ptrs {
		objs = append(objs, lfsObject{OID: p.OID, Size: p.Size})
	}
	body, err := json.Marshal(map[string]any{
		"operation": operation,
		"transfers": []string{"basic"},
		"objects":   objs,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/objects/batch"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", lfsMediaType)
	req.Header.Set("Accept", lfsMediaType)
	req.SetBasicAuth(c.remote.User, c.remote.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, lfsStatus(resp)
	}
	var answer struct {
		Objects []lfsObject `json:"objects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, err
	}
	return answer.Objects, nil
}

// download writes one object's bytes to w.
func (c *lfsClient) download(ctx context.Context, p Pointer, w io.Writer) error {
	objs, err := c.batch(ctx, "download", []Pointer{p})
	if err != nil {
		return err
	}
	if len(objs) != 1 {
		return fmt.Errorf("the batch API answered about %d objects, not one", len(objs))
	}
	o := objs[0]
	if o.Error != nil {
		return o.Error
	}
	action, ok := o.Actions["download"]
	if !ok {
		return fmt.Errorf("the node has no download for object %s", p.OID)
	}
	req, err := c.action(ctx, http.MethodGet, action, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lfsStatus(resp)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return err
	}
	if n != p.Size {
		return fmt.Errorf("object %s: got %d bytes, the pointer says %d", p.OID, n, p.Size)
	}
	return nil
}

// upload sends one object's bytes, and verifies it if the node asks.
func (c *lfsClient) upload(ctx context.Context, p Pointer, r io.Reader) error {
	objs, err := c.batch(ctx, "upload", []Pointer{p})
	if err != nil {
		return err
	}
	if len(objs) != 1 {
		return fmt.Errorf("the batch API answered about %d objects, not one", len(objs))
	}
	o := objs[0]
	if o.Error != nil {
		return o.Error
	}
	action, ok := o.Actions["upload"]
	if !ok {
		return nil // it arrived in the meantime
	}
	req, err := c.action(ctx, http.MethodPut, action, r)
	if err != nil {
		return err
	}
	req.ContentLength = p.Size
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return lfsStatus(resp)
	}
	io.Copy(io.Discard, resp.Body)
	verify, ok := o.Actions["verify"]
	if !ok {
		return nil
	}
	body, err := json.Marshal(map[string]any{"oid": p.OID, "size": p.Size})
	if err != nil {
		return err
	}
	req, err = c.action(ctx, http.MethodPost, verify, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", lfsMediaType)
	req.Header.Set("Accept", lfsMediaType)
	resp, err = c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return lfsStatus(resp)
	}
	return nil
}

// action builds the request the batch answer described. The server names
// the headers to send; without any, the token goes as basic auth, since
// the href is the same node.
func (c *lfsClient) action(ctx context.Context, method string, a lfsAction, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.Href, body)
	if err != nil {
		return nil, err
	}
	for k, v := range a.Header {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Authorization") == "" {
		req.SetBasicAuth(c.remote.User, c.remote.Token)
	}
	return req, nil
}

// lfsStatus turns a refusal into an error that says what the node said.
// A node with LFS turned off answers 404 to the batch endpoint, which is
// worth reading as such rather than as "no such repository".
func lfsStatus(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(body))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	if resp.StatusCode == http.StatusNotFound && msg == "" {
		msg = "not found; LFS may be turned off on this node"
	}
	return fmt.Errorf("%s: %s", resp.Status, msg)
}
