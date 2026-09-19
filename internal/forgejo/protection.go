package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// BranchProtection is a repository's rule for a branch or a pattern of
// them (modules/structs/repo_branch.go). Only the parts ForgeSync carries
// are here.
type BranchProtection struct {
	RuleName                string   `json:"rule_name"`
	EnablePush              bool     `json:"enable_push"`
	EnablePushWhitelist     bool     `json:"enable_push_whitelist"`
	PushWhitelistUsernames  []string `json:"push_whitelist_usernames"`
	PushWhitelistTeams      []string `json:"push_whitelist_teams"`
	PushWhitelistDeployKeys bool     `json:"push_whitelist_deploy_keys"`
	EnableMergeWhitelist    bool     `json:"enable_merge_whitelist"`
	MergeWhitelistUsernames []string `json:"merge_whitelist_usernames"`
	EnableStatusCheck       bool     `json:"enable_status_check"`
	StatusCheckContexts     []string `json:"status_check_contexts"`
	RequiredApprovals       int64    `json:"required_approvals"`
	BlockOnRejectedReviews  bool     `json:"block_on_rejected_reviews"`
	BlockOnOutdatedBranch   bool     `json:"block_on_outdated_branch"`
	ProtectedFilePatterns   string   `json:"protected_file_patterns"`
	UnprotectedFilePatterns string   `json:"unprotected_file_patterns"`
	ApplyToAdmins           bool     `json:"apply_to_admins"`
}

// BranchProtections lists a repository's rules.
func (c *Client) BranchProtections(ctx context.Context, owner, repo string) ([]BranchProtection, error) {
	var out []BranchProtection
	err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/branch_protections", true, nil, &out)
	return out, err
}

// CreateBranchProtection adds one.
func (c *Client) CreateBranchProtection(ctx context.Context, owner, repo string, r BranchProtection) (BranchProtection, error) {
	var out BranchProtection
	err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/branch_protections", true, r, &out)
	return out, err
}

// EditBranchProtection changes one, leaving its name as it is.
func (c *Client) EditBranchProtection(ctx context.Context, owner, repo, rule string, r BranchProtection) error {
	r.RuleName = "" // the name is in the path; Forgejo won't rename here
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("%s/branch_protections/%s", repoPath(owner, repo), url.PathEscape(rule)), true, r, nil)
}

// DeleteBranchProtection removes one. Already gone is not an error.
func (c *Client) DeleteBranchProtection(ctx context.Context, owner, repo, rule string) error {
	err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("%s/branch_protections/%s", repoPath(owner, repo), url.PathEscape(rule)), true, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}
