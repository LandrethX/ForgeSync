package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ActionVariable is a value a repository's workflows can read. Unlike a
// secret, it can be read back, which is what makes it replicable.
type ActionVariable struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

// ActionSecret is a secret as far as the API will say: its name and when
// it was set. The value is write-only and never comes back.
type ActionSecret struct {
	Name string `json:"name"`
}

// ActionVariables lists a repository's variables with their values.
func (c *Client) ActionVariables(ctx context.Context, owner, repo string) ([]ActionVariable, error) {
	var out []ActionVariable
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/actions/variables?limit=100", true, nil, &out)
	return out, err
}

// CreateActionVariable adds one.
func (c *Client) CreateActionVariable(ctx context.Context, owner, repo, name, value string) error {
	return c.do(ctx, http.MethodPost, variablePath(owner, repo, name), true,
		map[string]string{"value": value}, nil)
}

// UpdateActionVariable changes one's value, leaving its name.
func (c *Client) UpdateActionVariable(ctx context.Context, owner, repo, name, value string) error {
	return c.do(ctx, http.MethodPut, variablePath(owner, repo, name), true,
		map[string]string{"value": value}, nil)
}

// DeleteActionVariable removes one. Already gone is not an error.
func (c *Client) DeleteActionVariable(ctx context.Context, owner, repo, name string) error {
	err := c.do(ctx, http.MethodDelete, variablePath(owner, repo, name), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func variablePath(owner, repo, name string) string {
	return fmt.Sprintf("%s/actions/variables/%s", repoPath(owner, repo), url.PathEscape(name))
}

// ActionSecrets lists a repository's secrets by name. Their values are
// write-only: Forgejo never gives one back, so ForgeSync can compare which
// secrets a node has but can never copy what is in them.
func (c *Client) ActionSecrets(ctx context.Context, owner, repo string) ([]ActionSecret, error) {
	var out []ActionSecret
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/actions/secrets?limit=100", true, nil, &out)
	return out, err
}
