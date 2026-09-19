package issues

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/store"
)

// Labels and milestones are repository-level items that issues refer to.
// They follow the same rules as issues: one identity with a copy per node;
// fields merged against the last value that agreed everywhere; new on any
// node means new everywhere; deleted on the primary means deleted where
// unchanged; deleted elsewhere means recreated. Forgejo sends no webhook
// when they're created or edited (Phase 0 p05), so every issue run reads
// them. Only a repository's own labels take part; labels an organization
// provides are the same object everywhere already and are left alone.

// itemKind describes labels or milestones.
type itemKind struct {
	name   string   // "label" or "milestone": the store's kind
	key    string   // the field that names it, for matching and messages
	fields []string // merged fields
}

var (
	labelKind     = itemKind{"label", "name", []string{"name", "color", "description", "exclusive", "archived"}}
	milestoneKind = itemKind{"milestone", "title", []string{"title", "description", "state", "due"}}
)

func labelFields(l forgejo.Label) map[string]string {
	return map[string]string{
		"name": l.Name, "color": strings.ToLower(strings.TrimPrefix(l.Color, "#")), "description": l.Description,
		"exclusive": strconv.FormatBool(l.Exclusive), "archived": strconv.FormatBool(l.IsArchived),
	}
}

func milestoneFields(m forgejo.Milestone) map[string]string {
	due := ""
	if m.Deadline != nil && !m.Deadline.IsZero() {
		due = m.Deadline.UTC().Format(time.RFC3339)
	}
	return map[string]string{"title": m.Title, "description": m.Description, "state": m.State, "due": due}
}

// errCantClear: Forgejo's API can set a milestone's due date but not remove
// it.
//
//lint:ignore ST1005 the sentence starts with a proper noun
var errCantClear = errors.New("Forgejo's API can't remove a milestone's due date")

func (r *run) createItem(ctx context.Context, k itemKind, node string, f map[string]string) (int64, error) {
	api := r.s.nodes[node].API
	if k.name == "label" {
		l, err := api.CreateLabel(ctx, r.owner, r.name, forgejo.Label{Name: f["name"], Color: f["color"],
			Description: f["description"], Exclusive: f["exclusive"] == "true", IsArchived: f["archived"] == "true"})
		return l.ID, err
	}
	m := forgejo.Milestone{Title: f["title"], Description: f["description"], State: f["state"]}
	if f["due"] != "" {
		if t, err := time.Parse(time.RFC3339, f["due"]); err == nil {
			m.Deadline = &t
		}
	}
	created, err := api.CreateMilestone(ctx, r.owner, r.name, m)
	return created.ID, err
}

func (r *run) editItem(ctx context.Context, k itemKind, node string, id int64, field, value string) error {
	api := r.s.nodes[node].API
	if k.name == "label" {
		key := map[string]string{"archived": "is_archived"}[field]
		if key == "" {
			key = field
		}
		var v any = value
		if field == "exclusive" || field == "archived" {
			v = value == "true"
		}
		return api.EditLabel(ctx, r.owner, r.name, id, map[string]any{key: v})
	}
	if field == "due" {
		if value == "" {
			return errCantClear
		}
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return err
		}
		return api.EditMilestone(ctx, r.owner, r.name, id, map[string]any{"due_on": t})
	}
	return api.EditMilestone(ctx, r.owner, r.name, id, map[string]any{field: value})
}

func (r *run) deleteItem(ctx context.Context, k itemKind, node string, id int64) error {
	if k.name == "label" {
		return r.s.nodes[node].API.DeleteLabel(ctx, r.owner, r.name, id)
	}
	return r.s.nodes[node].API.DeleteMilestone(ctx, r.owner, r.name, id)
}

// items brings one kind of item together across the nodes and returns the
// records that still exist.
func (r *run) items(ctx context.Context, k itemKind, recs []store.RepoItem) ([]store.RepoItem, error) {
	known := map[string]map[int64]bool{}
	for _, n := range r.nodes {
		known[n] = map[int64]bool{}
	}
	for _, it := range recs {
		for n, fid := range it.Copies {
			if known[n] != nil {
				known[n][fid] = true
			}
		}
	}
	type fresh struct {
		node   string
		fid    int64
		fields map[string]string
	}
	var news []fresh
	for _, n := range r.nodes { // the primary first
		have := r.snaps[n].items[k.name]
		fids := make([]int64, 0, len(have))
		for fid := range have {
			fids = append(fids, fid)
		}
		sort.Slice(fids, func(i, j int) bool { return fids[i] < fids[j] }) // creation order on the node
		for _, fid := range fids {
			if !known[n][fid] {
				news = append(news, fresh{n, fid, have[fid]})
			}
		}
	}
	for _, f := range news {
		attached := false
		for i := range recs {
			it := &recs[i]
			if _, has := it.Copies[f.node]; has || !strings.EqualFold(r.currentItemKey(k, *it), f.fields[k.key]) {
				continue
			}
			it.Copies[f.node] = f.fid
			if f.node == r.primary {
				it.DeletedAt = nil // recreated on the primary to keep it
			}
			if _, err := r.s.store.SaveRepoItem(ctx, *it); err != nil {
				return nil, err
			}
			attached = true
			break
		}
		if attached {
			continue
		}
		it := store.RepoItem{RepositoryID: r.rec.ID, Kind: k.name, OriginNode: f.node, Base: f.fields,
			Copies: map[string]int64{f.node: f.fid}}
		id, err := r.s.store.SaveRepoItem(ctx, it)
		if err != nil {
			return nil, err
		}
		it.ID = id
		recs = append(recs, it)
	}

	var kept []store.RepoItem
	for i := range recs {
		gone, err := r.item(ctx, k, &recs[i])
		if err != nil {
			return nil, err
		}
		if !gone {
			kept = append(kept, recs[i])
		}
	}
	return kept, nil
}

func (r *run) currentItemKey(k itemKind, it store.RepoItem) string {
	for _, n := range r.nodes {
		if fid, ok := it.Copies[n]; ok {
			if f, ok := r.snaps[n].items[k.name][fid]; ok {
				return f[k.key]
			}
		}
	}
	return it.Base[k.key]
}

// item brings one label or milestone's copies together. gone reports that
// the record was deleted.
func (r *run) item(ctx context.Context, k itemKind, it *store.RepoItem) (bool, error) {
	ref := k.name + " " + it.Base[k.key]
	present := map[string]map[string]string{}
	deleted := it.DeletedAt != nil
	for _, n := range r.nodes {
		fid, mapped := it.Copies[n]
		if !mapped || deleted {
			continue
		}
		if f, ok := r.snaps[n].items[k.name][fid]; ok {
			present[n] = f
			continue
		}
		if n != r.primary {
			delete(it.Copies, n) // deleted there: recreated below
			r.itemVanished(n)
			continue
		}
		deleted = true
	}
	if deleted {
		if it.DeletedAt == nil {
			now := r.s.now().UTC()
			it.DeletedAt = &now
		}
		delete(it.Copies, r.primary)
		for _, n := range r.nodes {
			fid, ok := it.Copies[n]
			if !ok {
				continue
			}
			f, there := r.snaps[n].items[k.name][fid]
			if !there {
				delete(it.Copies, n)
				continue
			}
			if !sameFields(f, it.Base, k.fields) {
				r.conflict(ref+" deleted", map[string]any{"field": k.name + " deleted", "item": it.Base[k.key], "node": n})
				r.itemKept(n, it.ID)
				continue
			}
			if err := r.deleteItem(ctx, k, n, fid); err != nil {
				r.s.log.Warn("issues: deleting a "+k.name+" copy failed", "repository", r.rec.FullName, "node", n, "error", err)
				r.complete = false
				continue
			}
			r.s.log.Info(k.name+" deleted as on the primary", "repository", r.rec.FullName, "node", n, k.key, it.Base[k.key])
			delete(it.Copies, n)
		}
		if len(it.Copies) == 0 {
			return true, r.s.store.DeleteRepoItem(ctx, it.ID)
		}
		_, err := r.s.store.SaveRepoItem(ctx, *it)
		return false, err
	}
	if len(present) == 0 {
		return false, nil
	}

	want := map[string]string{}
	base := map[string]string{}
	for k2, v := range it.Base {
		base[k2] = v
	}
	for _, field := range k.fields {
		vals := map[string]string{}
		for n, f := range present {
			vals[n] = f[field]
		}
		value, writes, conflict := merge(vals, base[field])
		if conflict {
			r.conflict(ref+" "+field, map[string]any{"field": k.name + " " + field, "item": it.Base[k.key], "values": clip(vals)})
			if f, ok := present[r.primary]; ok {
				want[field] = f[field]
			} else {
				want[field] = base[field]
			}
			continue
		}
		want[field] = value
		for _, n := range writes {
			if err := r.editItem(ctx, k, n, it.Copies[n], field, value); err != nil {
				level := r.s.log.Warn
				if errors.Is(err, errCantClear) {
					level = r.s.log.Debug
				}
				level("issues: updating a "+k.name+" copy failed", "repository", r.rec.FullName, "node", n, "field", field, "error", err)
				r.complete = false
				continue
			}
			r.s.log.Info(k.name+" updated", "repository", r.rec.FullName, "node", n, k.key, it.Base[k.key],
				"field", field, "value", value)
		}
		base[field] = value
	}
	it.Base = base

	for _, n := range r.nodes {
		if _, has := it.Copies[n]; has {
			continue
		}
		fid, err := r.createItem(ctx, k, n, want)
		if err != nil {
			r.s.log.Warn("issues: creating a "+k.name+" copy failed", "repository", r.rec.FullName, "node", n, "error", err)
			r.complete = false
			continue
		}
		it.Copies[n] = fid
		r.snaps[n].items[k.name][fid] = want
		r.s.log.Info(k.name+" copied", "repository", r.rec.FullName, "node", n, k.key, want[k.key])
		if _, err := r.s.store.SaveRepoItem(ctx, *it); err != nil {
			return false, err
		}
	}
	_, err := r.s.store.SaveRepoItem(ctx, *it)
	return false, err
}

// itemVanished notes that a label or milestone was deleted on this node and
// is being recreated. Forgejo took it off that node's issues as well, and
// the snapshot was read before the copy came back, so the node can't be
// compared on labels or milestones in this run: leaving it out keeps a
// deletion there from being read as someone unlabelling the issues, and the
// next run sees the truth.
func (r *run) itemVanished(node string) {
	if r.restored == nil {
		r.restored = map[string]bool{}
	}
	r.restored[node] = true
	r.complete = false
}

// itemKept notes that this node's copy of a label or milestone deleted on
// the primary was changed there, so it stays for its owner to settle. The
// issues on that node keep it too: ForgeSync neither reads it as something
// the other nodes lost nor takes it off them.
func (r *run) itemKept(node, item string) {
	if r.kept == nil {
		r.kept = map[string]map[string]bool{}
	}
	if r.kept[node] == nil {
		r.kept[node] = map[string]bool{}
	}
	r.kept[node][item] = true
}

func sameFields(a, b map[string]string, fields []string) bool {
	for _, f := range fields {
		if a[f] != b[f] {
			return false
		}
	}
	return true
}

// itemIndex maps each node's Forgejo ids of labels or milestones to item
// ids, and back.
type itemIndex struct {
	toItem map[string]map[int64]string // node -> Forgejo id -> item id
	toFID  map[string]map[string]int64 // node -> item id -> Forgejo id
}

func newItemIndex(items []store.RepoItem) itemIndex {
	x := itemIndex{toItem: map[string]map[int64]string{}, toFID: map[string]map[string]int64{}}
	for _, it := range items {
		for n, fid := range it.Copies {
			if x.toItem[n] == nil {
				x.toItem[n], x.toFID[n] = map[int64]string{}, map[string]int64{}
			}
			x.toItem[n][fid], x.toFID[n][it.ID] = it.ID, fid
		}
	}
	return x
}

// labelsValue is an issue's labels on a node as merge sees them: the
// sorted item ids of the repository labels it has, leaving out any kept
// there for their owner, which are none of ForgeSync's business.
func (x itemIndex) labelsValue(node string, is forgejo.Issue, kept map[string]bool) string {
	var ids []string
	for _, l := range is.Labels {
		if id, ok := x.toItem[node][l.ID]; ok && !kept[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func (x itemIndex) milestoneValue(node string, is forgejo.Issue) string {
	if is.Milestone == nil {
		return ""
	}
	return x.toItem[node][is.Milestone.ID]
}

// labelFIDs translates a labels value to a node's label ids, keeping the
// labels the issue has there that aren't ForgeSync's to move: the ones that
// aren't the repository's own (an organization's, say) and the ones kept
// for their owner. ok is false if a label has no copy there yet.
func (x itemIndex) labelFIDs(node, value string, is *forgejo.Issue, kept map[string]bool) (fids []int64, ok bool) {
	if value != "" {
		for _, id := range strings.Split(value, ",") {
			fid, has := x.toFID[node][id]
			if !has {
				return nil, false
			}
			fids = append(fids, fid)
		}
	}
	if is != nil {
		for _, l := range is.Labels {
			if id, repoLabel := x.toItem[node][l.ID]; !repoLabel || kept[id] {
				fids = append(fids, l.ID)
			}
		}
	}
	return fids, true
}

func (x itemIndex) milestoneFID(node, value string) (int64, bool) {
	if value == "" {
		return 0, true
	}
	fid, ok := x.toFID[node][value]
	return fid, ok
}
