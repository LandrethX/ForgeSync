package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Release is a published release on a tag. The tag is what identifies it
// across the nodes: git replication has already put the same tag on each.
type Release struct {
	ID         int64          `json:"id"`
	TagName    string         `json:"tag_name"`
	Target     string         `json:"target_commitish"`
	Title      string         `json:"name"`
	Body       string         `json:"body"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Author     User           `json:"author"`
	Created    time.Time      `json:"created_at"`
	Assets     []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is a file published with a release.
type ReleaseAsset struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"browser_download_url"`
}

// Tags lists a repository's tag names. A release can only be published on
// a node that already has its tag.
func (c *Client) Tags(ctx context.Context, owner, repo string) ([]string, error) {
	var out []struct {
		Name string `json:"name"`
	}
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/tags?limit=200", true, nil, &out)
	names := make([]string, 0, len(out))
	for _, t := range out {
		names = append(names, t.Name)
	}
	return names, err
}

// Releases lists a repository's releases. The draft and pre-release query
// parameters are filters, not switches -- draft=true returns the drafts
// and nothing else -- so neither is sent, and the caller decides what to
// do with a draft it finds.
func (c *Client) Releases(ctx context.Context, owner, repo string, page, limit int) ([]Release, error) {
	var out []Release
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/releases?page=%d&limit=%d", repoPath(owner, repo), page, limit), true, nil, &out)
	return out, err
}

// CreateRelease publishes one as the client's user (Sudo picks who). The
// tag has to be on the node already.
func (c *Client) CreateRelease(ctx context.Context, owner, repo string, r Release) (Release, error) {
	var out Release
	body := map[string]any{"tag_name": r.TagName, "name": r.Title, "body": r.Body,
		"draft": r.Draft, "prerelease": r.Prerelease}
	if r.Target != "" {
		body["target_commitish"] = r.Target
	}
	err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/releases", true, body, &out)
	return out, err
}

// EditRelease changes what a release says, leaving its tag and its assets
// where they are.
func (c *Client) EditRelease(ctx context.Context, owner, repo string, id int64, r Release) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/releases/%d", repoPath(owner, repo), id), true,
		map[string]any{"name": r.Title, "body": r.Body, "draft": r.Draft, "prerelease": r.Prerelease}, nil)
}

// DeleteRelease removes one, leaving the tag. Already gone is not an error.
func (c *Client) DeleteRelease(ctx context.Context, owner, repo string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/releases/%d", repoPath(owner, repo), id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// UploadReleaseAsset publishes a file with a release.
func (c *Client) UploadReleaseAsset(ctx context.Context, owner, repo string, release int64, name string, content []byte) (ReleaseAsset, error) {
	a, err := c.upload(ctx, fmt.Sprintf("%s/releases/%d/assets", repoPath(owner, repo), release), name, content)
	return ReleaseAsset{ID: a.ID, Name: a.Name, Size: a.Size, DownloadURL: a.DownloadURL}, err
}

// DeleteReleaseAsset removes one. Already gone is not an error.
func (c *Client) DeleteReleaseAsset(ctx context.Context, owner, repo string, release, asset int64) error {
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("%s/releases/%d/assets/%d", repoPath(owner, repo), release, asset), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}
