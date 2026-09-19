package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Package is one version of one package in a node's registry. Packages
// belong to an owner (a person or an organization), not to a repository,
// although one can be linked to a repository for its page.
type Package struct {
	ID         int64       `json:"id"`
	Owner      User        `json:"owner"`
	Repository *Repository `json:"repository"`
	Creator    User        `json:"creator"`
	Type       string      `json:"type"`
	Name       string      `json:"name"`
	Version    string      `json:"version"`
	HTMLURL    string      `json:"html_url"`
}

// PackageFile is one file of a package version. The digests are the
// registry's own, so a file means the same thing on every node.
type PackageFile struct {
	ID         int64  `json:"id"`
	Size       int64  `json:"Size"`
	Name       string `json:"name"`
	HashSHA256 string `json:"sha256"`
}

// Packages lists one owner's packages, one page at a time.
func (c *Client) Packages(ctx context.Context, owner string, page, limit int) ([]Package, error) {
	var out []Package
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/api/v1/packages/%s?page=%d&limit=%d", url.PathEscape(owner), page, limit), true, nil, &out)
	return out, err
}

// PackageFiles lists the files of one package version.
func (c *Client) PackageFiles(ctx context.Context, owner, typ, name, version string) ([]PackageFile, error) {
	var out []PackageFile
	err := c.do(ctx, http.MethodGet, "/api/v1/packages/"+url.PathEscape(owner)+"/"+url.PathEscape(typ)+
		"/"+url.PathEscape(name)+"/"+url.PathEscape(version)+"/files", true, nil, &out)
	return out, err
}

// DeletePackage removes one package version and everything in it.
func (c *Client) DeletePackage(ctx context.Context, owner, typ, name, version string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/packages/"+url.PathEscape(owner)+"/"+url.PathEscape(typ)+
		"/"+url.PathEscape(name)+"/"+url.PathEscape(version), true, nil, nil)
}
