package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"scenegit.org/forgesync/internal/buildinfo"
)

// Attachment is a file on an issue or a comment
// (modules/structs/attachment.go). Type is "attachment" for an uploaded
// file and "external" for one that is only a link.
type Attachment struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	Created     time.Time `json:"created_at"`
	UUID        string    `json:"uuid"`
	DownloadURL string    `json:"browser_download_url"`
	Type        string    `json:"type"`
}

// IssueAttachments lists an issue's attachments.
func (c *Client) IssueAttachments(ctx context.Context, owner, repo string, number int64) ([]Attachment, error) {
	var out []Attachment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d/assets", repoPath(owner, repo), number), true, nil, &out)
	return out, err
}

// CommentAttachments lists a comment's attachments.
func (c *Client) CommentAttachments(ctx context.Context, owner, repo string, id int64) ([]Attachment, error) {
	var out []Attachment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/comments/%d/assets", repoPath(owner, repo), id), true, nil, &out)
	return out, err
}

// DeleteIssueAttachment removes one. Already gone is not an error.
func (c *Client) DeleteIssueAttachment(ctx context.Context, owner, repo string, number, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/%d/assets/%d", repoPath(owner, repo), number, id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// DeleteCommentAttachment removes one from a comment.
func (c *Client) DeleteCommentAttachment(ctx context.Context, owner, repo string, commentID, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/comments/%d/assets/%d", repoPath(owner, repo), commentID, id), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// UploadIssueAttachment attaches content to an issue as the client's user
// (Sudo picks who). Forgejo refuses a file type the node doesn't allow or
// one over its size limit.
func (c *Client) UploadIssueAttachment(ctx context.Context, owner, repo string, number int64, name string, content []byte) (Attachment, error) {
	return c.upload(ctx, fmt.Sprintf("%s/issues/%d/assets", repoPath(owner, repo), number), name, content)
}

// UploadCommentAttachment attaches content to a comment.
func (c *Client) UploadCommentAttachment(ctx context.Context, owner, repo string, id int64, name string, content []byte) (Attachment, error) {
	return c.upload(ctx, fmt.Sprintf("%s/issues/comments/%d/assets", repoPath(owner, repo), id), name, content)
}

func (c *Client) upload(ctx context.Context, path, name string, content []byte) (Attachment, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	// The handler reads the file from the "attachment" part and takes the
	// name from the query, so a name it can't put in a header is still kept.
	part, err := w.CreateFormFile("attachment", "upload")
	if err != nil {
		return Attachment{}, err
	}
	if _, err := part.Write(content); err != nil {
		return Attachment{}, err
	}
	if err := w.Close(); err != nil {
		return Attachment{}, err
	}
	full := c.base.String() + path + "?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, &body)
	if err != nil {
		return Attachment{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", "forgesync/"+buildinfo.Version)
	req.Header.Set("Authorization", "token "+c.token)
	if c.sudo != "" {
		req.Header.Set("Sudo", c.sudo)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Attachment{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Attachment{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Method: http.MethodPost, Path: path, StatusCode: resp.StatusCode}
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &m) == nil {
			apiErr.Message = m.Message
		}
		return Attachment{}, apiErr
	}
	var out Attachment
	if err := json.Unmarshal(raw, &out); err != nil {
		return Attachment{}, fmt.Errorf("forgejo POST %s: decode response: %w", path, err)
	}
	return out, nil
}

// Download fetches an attachment's content from the URL Forgejo gives for
// it, which is a web route rather than an API one. It refuses anything
// larger than max, so one enormous file can't take the controller down.
func (c *Client) Download(ctx context.Context, rawURL string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "forgesync/"+buildinfo.Version)
	// Go drops the Authorization header if a redirect leaves this host.
	req.Header.Set("Authorization", "token "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Method: http.MethodGet, Path: rawURL, StatusCode: resp.StatusCode}
	}
	// One byte over the limit tells us it was too big rather than truncating.
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("forgejo GET %s: larger than %d bytes", rawURL, max)
	}
	return b, nil
}
