package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
	"scenegit.org/forgesync/internal/store"
)

// A release is what someone published on a tag, and the tag is what
// identifies it everywhere: git replication has already put the same tag
// on every node, so nothing has to be mapped.
//
// Releases merge member by member (internal/set) as
// "<tag>:<digest of what it says>". One published anywhere is published
// everywhere, as the person who published it; one removed anywhere is
// removed everywhere. Changing the words is one member going and another
// arriving, and because both land on the same node in one run it is
// written as an edit rather than a fresh release -- which also means its
// files aren't thrown away and uploaded again.
//
// The files travel as their own members, "<tag>|<size>|<name>", the way an
// issue's attachments do: downloaded from a node that has one and uploaded
// to the nodes that don't, never bigger than AssetMax.
//
// A draft is not published, so it doesn't travel. A release whose tag
// hasn't reached a node yet waits there: replication puts the tag on
// first, and the next run publishes it.

// releaseMember identifies a release and what it says.
func releaseMember(r forgejo.Release) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\n%s\n%v\n%v", r.Title, r.Body, r.Draft, r.Prerelease))
	return r.TagName + ":" + hex.EncodeToString(sum[:])[:12]
}

// releaseTagOf is the tag in a member.
func releaseTagOf(member string) string {
	i := strings.LastIndex(member, ":")
	if i < 0 {
		return member
	}
	return member[:i]
}

// assetMember identifies one of a release's files.
func assetMember(tag string, a forgejo.ReleaseAsset) string {
	return tag + "|" + strconv.FormatInt(a.Size, 10) + "|" + a.Name
}

func assetParts(member string) (tag string, size int64, name string) {
	parts := strings.SplitN(member, "|", 3)
	if len(parts) != 3 {
		return "", 0, ""
	}
	size, _ = strconv.ParseInt(parts[1], 10, 64)
	return parts[0], size, parts[2]
}

// nodeReleases is what one node has published.
type nodeReleases struct {
	byMember map[string]forgejo.Release // release member -> release
	byTag    map[string]forgejo.Release // tag -> release, for editing in place
	assets   map[string]forgejo.ReleaseAsset
	tags     map[string]bool
}

// syncReleases brings one repository's releases and their files together.
func (e *Engine) syncReleases(ctx context.Context, rec store.RepositoryRecord, healthy map[string]bool) {
	owner, name, ok := strings.Cut(rec.FullName, "/")
	if !ok {
		return
	}
	at := map[string]*nodeReleases{}
	have := map[string]map[string]bool{}
	files := map[string]map[string]bool{}
	for _, n := range e.order {
		if !healthy[n] || e.nodes[n].API == nil || !e.hasRepo(rec, n) {
			continue
		}
		st, err := e.readReleases(ctx, e.nodes[n], owner, name)
		if err != nil {
			e.log.Warn("releases: reading them failed", "repository", rec.FullName, "node", n, "error", err)
			continue
		}
		at[n] = st
		have[n], files[n] = map[string]bool{}, map[string]bool{}
		for m := range st.byMember {
			have[n][m] = true
		}
		for m := range st.assets {
			files[n][m] = true
		}
	}
	if len(at) < 2 {
		return
	}
	e.mergeReleases(ctx, rec, owner, name, at, have)
	e.mergeAssets(ctx, rec, owner, name, at, files)

	if err := e.store.SetRepositoryReleases(ctx, rec.ID,
		set.Settle(have, set.From(rec.BaseReleases)), set.Settle(files, set.From(rec.BaseReleaseAssets))); err != nil {
		e.log.Error("releases: recording them failed", "repository", rec.FullName, "error", err)
	}
}

func (e *Engine) readReleases(ctx context.Context, n Node, owner, name string) (*nodeReleases, error) {
	st := &nodeReleases{byMember: map[string]forgejo.Release{}, byTag: map[string]forgejo.Release{},
		assets: map[string]forgejo.ReleaseAsset{}, tags: map[string]bool{}}
	for page := 1; ; page++ {
		list, err := n.API.Releases(ctx, owner, name, page, 50)
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			if r.Draft {
				continue // not published, so not ForgeSync's to spread
			}
			st.byMember[releaseMember(r)], st.byTag[r.TagName] = r, r
			for _, a := range r.Assets {
				st.assets[assetMember(r.TagName, a)] = a
			}
		}
		if len(list) < 50 {
			break
		}
	}
	tags, err := n.API.Tags(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	for _, t := range tags {
		st.tags[t] = true
	}
	return st, nil
}

func (e *Engine) mergeReleases(ctx context.Context, rec store.RepositoryRecord, owner, name string,
	at map[string]*nodeReleases, have map[string]map[string]bool) {
	plan := set.Decide(have, set.From(rec.BaseReleases))
	for _, n := range e.order {
		st, taking := at[n]
		if !taking {
			continue
		}
		editing := map[string]bool{}
		for _, m := range plan.Add[n] {
			editing[releaseTagOf(m)] = true
		}
		for _, m := range plan.Remove[n] {
			tag := releaseTagOf(m)
			// A release whose words are only changing is edited below, so
			// its files aren't thrown away and uploaded again.
			if editing[tag] {
				delete(have[n], m)
				continue
			}
			r, ok := st.byTag[tag]
			if !ok {
				continue
			}
			if err := e.nodes[n].API.DeleteRelease(ctx, owner, name, r.ID); err != nil {
				e.log.Warn("releases: removing one failed; left for the next run", "repository", rec.FullName,
					"node", n, "tag", tag, "error", err)
				continue
			}
			delete(st.byTag, tag)
			delete(have[n], m)
			e.log.Info("release removed", "repository", rec.FullName, "node", n, "tag", tag)
			e.audit(ctx, "repo.release_removed", rec.FullName,
				map[string]any{"repository_id": rec.ID, "node": n, "tag": tag})
		}
		for _, m := range plan.Add[n] {
			want, from, found := releaseFrom(e.order, at, m)
			if !found {
				continue
			}
			tag := releaseTagOf(m)
			if !st.tags[tag] {
				e.log.Debug("releases: waiting for the tag", "repository", rec.FullName, "node", n, "tag", tag)
				continue
			}
			if old, there := st.byTag[tag]; there {
				if err := e.nodes[n].API.EditRelease(ctx, owner, name, old.ID, want); err != nil {
					e.log.Warn("releases: changing one failed; left for the next run", "repository", rec.FullName,
						"node", n, "tag", tag, "error", err)
					continue
				}
				e.log.Info("release changed", "repository", rec.FullName, "node", n, "tag", tag, "from", from)
			} else {
				who := want.Author.Login
				if who != "" {
					if err := e.EnsureUser(ctx, who, from, n); err != nil {
						e.log.Info("releases: the author can't be created on the node; left for the next run",
							"repository", rec.FullName, "node", n, "tag", tag, "author", who, "error", err)
						continue
					}
				}
				made, err := e.publisher(e.nodes[n], who).CreateRelease(ctx, owner, name, want)
				if err != nil {
					e.log.Warn("releases: publishing one failed; left for the next run", "repository", rec.FullName,
						"node", n, "tag", tag, "error", err)
					continue
				}
				st.byTag[tag] = made
				e.log.Info("release published", "repository", rec.FullName, "node", n, "tag", tag, "from", from, "by", who)
				e.audit(ctx, "repo.release_published", rec.FullName,
					map[string]any{"repository_id": rec.ID, "node": n, "tag": tag, "by": who})
			}
			have[n][m] = true
		}
	}
}

func (e *Engine) mergeAssets(ctx context.Context, rec store.RepositoryRecord, owner, name string,
	at map[string]*nodeReleases, files map[string]map[string]bool) {
	plan := set.Decide(files, set.From(rec.BaseReleaseAssets))
	for _, n := range e.order {
		st, taking := at[n]
		if !taking {
			continue
		}
		for _, m := range plan.Remove[n] {
			tag, _, file := assetParts(m)
			r, ok := st.byTag[tag]
			a, has := st.assets[m]
			if !ok || !has {
				continue
			}
			if err := e.nodes[n].API.DeleteReleaseAsset(ctx, owner, name, r.ID, a.ID); err != nil {
				e.log.Warn("releases: removing a file failed; left for the next run", "repository", rec.FullName,
					"node", n, "tag", tag, "file", file, "error", err)
				continue
			}
			delete(st.assets, m)
			delete(files[n], m)
			e.log.Info("release file removed", "repository", rec.FullName, "node", n, "tag", tag, "file", file)
		}
		for _, m := range plan.Add[n] {
			tag, size, file := assetParts(m)
			r, ok := st.byTag[tag]
			if !ok {
				continue // its release isn't here yet; next run
			}
			if size > e.opts.AssetMax {
				e.log.Info("releases: a file is too big to copy; left where it is", "repository", rec.FullName,
					"node", n, "tag", tag, "file", file, "bytes", size, "limit", e.opts.AssetMax)
				continue
			}
			content, from, ok := e.fetchAsset(ctx, at, m)
			if !ok {
				e.log.Warn("releases: no node has the file to copy", "repository", rec.FullName, "tag", tag, "file", file)
				continue
			}
			a, err := e.nodes[n].API.UploadReleaseAsset(ctx, owner, name, r.ID, file, content)
			if err != nil {
				e.log.Warn("releases: copying a file failed; left for the next run", "repository", rec.FullName,
					"node", n, "tag", tag, "file", file, "error", err)
				continue
			}
			st.assets[m] = a
			files[n][m] = true
			e.log.Info("release file copied", "repository", rec.FullName, "node", n, "from", from,
				"tag", tag, "file", file, "bytes", len(content))
		}
	}
}

// fetchAsset reads a file from a node that has it, the primary first.
func (e *Engine) fetchAsset(ctx context.Context, at map[string]*nodeReleases, member string) ([]byte, string, bool) {
	for _, n := range e.order {
		st, ok := at[n]
		if !ok {
			continue
		}
		a, has := st.assets[member]
		if !has || a.DownloadURL == "" {
			continue
		}
		content, err := e.nodes[n].API.Download(ctx, a.DownloadURL, e.opts.AssetMax)
		if err != nil {
			continue
		}
		return content, n, true
	}
	return nil, "", false
}

func releaseFrom(order []string, at map[string]*nodeReleases, member string) (forgejo.Release, string, bool) {
	for _, n := range order {
		if st, ok := at[n]; ok {
			if r, has := st.byMember[member]; has {
				return r, n, true
			}
		}
	}
	return forgejo.Release{}, "", false
}

// publisher is the node's API acting as the person who published the
// release, so their name stays on it.
func (e *Engine) publisher(n Node, login string) NodeAPI {
	if n.As == nil || login == "" {
		return n.API
	}
	return n.As(login)
}
