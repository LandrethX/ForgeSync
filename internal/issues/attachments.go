package issues

import (
	"context"
	"strconv"
	"strings"

	"scenegit.org/forgesync/internal/forgejo"
	"scenegit.org/forgesync/internal/set"
)

// Attachments are the files on an issue or a comment. Forgejo gives each
// one its own id and uuid per node, and there's nothing in it that says two
// are the same file, so ForgeSync matches them by size and name --
// "<size>:<name>". Two files of the same size with the same name on one
// issue therefore count as one; nothing is lost, but the second isn't
// copied.
//
// They merge member by member (see set.go): one that appears anywhere is
// downloaded from a node that has it and uploaded to the nodes that don't,
// as the issue's or the comment's author; one missing anywhere is deleted
// from the nodes that still have it. A node can refuse an upload -- a file
// type it doesn't allow, a size over its limit -- and then the member stays
// out of the base and is tried again next run, so its absence is never read
// as someone deleting the file.
//
// Renaming a file looks like deleting one and adding another, so the file
// is uploaded again under the new name. That costs a transfer and loses
// nothing. Attachments that are only a link ("external") are left alone:
// Forgejo's API can't create one.

// attachmentMember is how an attachment is matched across the nodes.
func attachmentMember(a forgejo.Attachment) string {
	return strconv.FormatInt(a.Size, 10) + ":" + a.Name
}

// attachmentName is the file name in a member.
func attachmentName(member string) string {
	_, name, _ := strings.Cut(member, ":")
	return name
}

// attachmentSize is its size, from the member itself: ForgeSync can tell a
// file is too big to carry without fetching it.
func attachmentSize(member string) int64 {
	size, _, _ := strings.Cut(member, ":")
	n, _ := strconv.ParseInt(size, 10, 64)
	return n
}

// attachmentsOn is what one node has, by member. External ones are left
// out: ForgeSync can neither copy nor judge them.
func attachmentsOn(list []forgejo.Attachment) map[string]forgejo.Attachment {
	out := map[string]forgejo.Attachment{}
	for _, a := range list {
		if a.Type != "" && a.Type != "attachment" {
			continue
		}
		if _, taken := out[attachmentMember(a)]; !taken {
			out[attachmentMember(a)] = a
		}
	}
	return out
}

// attacher puts a file on a node, or takes one off it.
type attacher struct {
	upload func(ctx context.Context, node, author, name string, content []byte) error
	remove func(ctx context.Context, node string, a forgejo.Attachment) error
}

// syncAttachments brings one issue's or comment's files together and
// returns the new base. at maps a node to what it has.
func (r *run) syncAttachments(ctx context.Context, ref, author, base string,
	at map[string]map[string]forgejo.Attachment, act attacher) string {
	have := map[string]map[string]bool{}
	for n, m := range at {
		have[n] = map[string]bool{}
		for e := range m {
			have[n][e] = true
		}
	}
	plan := set.Decide(have, set.From(base))
	for _, n := range r.nodes {
		for _, e := range plan.Remove[n] {
			a := at[n][e]
			if err := act.remove(ctx, n, a); err != nil {
				r.s.log.Warn("issues: an attachment couldn't be removed; left for the next run",
					"repository", r.rec.FullName, "node", n, "issue", ref, "file", a.Name, "error", err)
				r.complete = false
				continue
			}
			delete(at[n], e)
			delete(have[n], e)
			r.s.log.Info("attachment removed", "repository", r.rec.FullName, "node", n, "issue", ref, "file", a.Name)
		}
		for _, e := range plan.Add[n] {
			if attachmentSize(e) > r.s.opts.AttachmentMax {
				r.s.log.Info("issues: an attachment is too big to copy; left where it is",
					"repository", r.rec.FullName, "node", n, "issue", ref, "file", attachmentName(e),
					"bytes", attachmentSize(e), "limit", r.s.opts.AttachmentMax)
				r.complete = false
				continue
			}
			content, from, err := r.fetchAttachment(ctx, at, e)
			if err != nil {
				r.s.log.Warn("issues: an attachment couldn't be read; left for the next run",
					"repository", r.rec.FullName, "issue", ref, "file", attachmentName(e), "error", err)
				r.complete = false
				continue
			}
			if err := act.upload(ctx, n, author, attachmentName(e), content); err != nil {
				r.s.log.Warn("issues: an attachment couldn't be copied; left for the next run",
					"repository", r.rec.FullName, "node", n, "from", from, "issue", ref,
					"file", attachmentName(e), "error", err)
				r.complete = false
				continue
			}
			if at[n] == nil {
				at[n] = map[string]forgejo.Attachment{}
			}
			at[n][e], have[n][e] = forgejo.Attachment{Name: attachmentName(e)}, true
			r.s.log.Info("attachment copied", "repository", r.rec.FullName, "node", n, "from", from,
				"issue", ref, "file", attachmentName(e), "bytes", len(content))
		}
	}
	return set.Settle(have, set.From(base))
}

// fetchAttachment reads a file from a node that has it, the primary first.
func (r *run) fetchAttachment(ctx context.Context, at map[string]map[string]forgejo.Attachment, member string) ([]byte, string, error) {
	var last error
	for _, n := range r.nodes { // the primary first
		a, ok := at[n][member]
		if !ok || a.DownloadURL == "" {
			continue
		}
		content, err := r.s.nodes[n].API.Download(ctx, a.DownloadURL, r.s.opts.AttachmentMax)
		if err != nil {
			last = err
			continue
		}
		return content, n, nil
	}
	if last == nil {
		last = errNoSource
	}
	return nil, "", last
}
