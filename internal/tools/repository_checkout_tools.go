package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func RegisterRepositoryCheckoutTools(r *Registry) {
	if r == nil {
		return
	}
	itemSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"repository_id":  map[string]interface{}{"type": "string", "description": "Stable repository ID returned by list_repositories. Preferred selector; copy it exactly, not the repository name or display name."},
			"repo_full_name": map[string]interface{}{"type": "string", "description": "Exact owner/repo full name returned by list_repositories. Do not pass a display name or bare repository name."},
			"base_branch":    map[string]interface{}{"type": "string", "description": "Optional base branch override."},
			"work_branch":    map[string]interface{}{"type": "string", "description": "Optional work branch override."},
			"alias":          map[string]interface{}{"type": "string", "description": "Optional short alias for selecting this repo in later read-only tools."},
			"primary":        map[string]interface{}{"type": "boolean", "description": "Make this the primary repository workspace. Defaults to true when no primary repo is checked out."},
		},
		"additionalProperties": false,
	}
	r.Register(Definition{
		Name:        "checkout_repositories",
		Description: "Checkout one to four connected repositories. For one repository already resolved by the current target, the item may omit selectors. Otherwise call list_repositories and pass repository_id (preferred) or exact repo_full_name; display names and bare repository names are not accepted. Use aliases to select repositories in later tools.",
		Category:    "Workspace",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"repositories": map[string]interface{}{
					"type":        "array",
					"description": "One to four repositories to checkout.",
					"minItems":    1,
					"maxItems":    4,
					"items":       itemSchema,
				},
			},
			"required":             []string{"repositories"},
			"additionalProperties": false,
		},
		Mutating: false,
	}, checkoutRepositories)
}

func checkoutRepository(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	req, err := parseCheckoutRepositoryRequest(input)
	if err != nil {
		return nil, err
	}
	result, err := checkoutRepositoryWithManager(ctx, callCtx, req)
	if err != nil {
		return nil, err
	}
	return json.Marshal(checkoutRepositoryToolResult(result))
}

func checkoutRepositories(ctx context.Context, callCtx CallContext, input json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Repositories []json.RawMessage `json:"repositories"`
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return nil, err
	}
	if len(params.Repositories) == 0 {
		return nil, fmt.Errorf("repositories is required")
	}
	if len(params.Repositories) > 4 {
		return nil, fmt.Errorf("too many repositories: max 4 per call")
	}
	results := make([]map[string]interface{}, 0, len(params.Repositories))
	for idx, raw := range params.Repositories {
		req, err := parseCheckoutRepositoryRequest(raw)
		if err != nil {
			return nil, fmt.Errorf("repositories[%d]: %w", idx, err)
		}
		if len(params.Repositories) > 1 && req.RepositoryID == "" && req.RepoFullName == "" {
			return nil, fmt.Errorf("repositories[%d]: repository_id or repo_full_name is required; call list_repositories and copy repository_id", idx)
		}
		result, err := checkoutRepositoryWithManager(ctx, callCtx, req)
		if err != nil {
			return nil, fmt.Errorf("repositories[%d]: %w", idx, err)
		}
		results = append(results, checkoutRepositoryToolResult(result))
	}
	return json.Marshal(map[string]interface{}{
		"checked_out":  len(results),
		"repositories": results,
	})
}

func parseCheckoutRepositoryRequest(input json.RawMessage) (CheckoutRepositoryRequest, error) {
	var params struct {
		RepositoryID string `json:"repository_id"`
		RepoFullName string `json:"repo_full_name"`
		BaseBranch   string `json:"base_branch"`
		WorkBranch   string `json:"work_branch"`
		Alias        string `json:"alias"`
		Primary      *bool  `json:"primary"`
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if err := decodeStrictWorkspaceInput(input, &params); err != nil {
		return CheckoutRepositoryRequest{}, err
	}
	req := CheckoutRepositoryRequest{
		RepositoryID: strings.TrimSpace(params.RepositoryID),
		RepoFullName: strings.TrimSpace(params.RepoFullName),
		BaseBranch:   strings.TrimSpace(params.BaseBranch),
		WorkBranch:   strings.TrimSpace(params.WorkBranch),
		Alias:        strings.TrimSpace(params.Alias),
	}
	if params.Primary != nil {
		req.Primary = *params.Primary
	}
	return req, nil
}

func checkoutRepositoryWithManager(ctx context.Context, callCtx CallContext, req CheckoutRepositoryRequest) (*CheckoutRepositoryResult, error) {
	if callCtx.WorkspaceManager == nil {
		return nil, fmt.Errorf("repository checkout is not configured for this runtime")
	}
	result, err := callCtx.WorkspaceManager.CheckoutRepository(ctx, req)
	if err != nil {
		if req.RepositoryID == "" && req.RepoFullName == "" {
			return nil, fmt.Errorf("%w; call list_repositories, then call checkout_repositories with repository_id or repo_full_name", err)
		}
		return nil, err
	}
	if result == nil || result.Lease == nil {
		return nil, fmt.Errorf("repository checkout did not return a workspace lease")
	}
	return result, nil
}

func checkoutRepositoryToolResult(result *CheckoutRepositoryResult) map[string]interface{} {
	out := map[string]interface{}{
		"primary": result.Primary,
	}
	if result.Alias != "" {
		out["alias"] = result.Alias
	}
	if result.RepositoryID != "" {
		out["repository_id"] = result.RepositoryID
	}
	if result.RepoFullName != "" {
		out["repo_full_name"] = result.RepoFullName
	}
	if result.BaseBranch != "" {
		out["base_branch"] = result.BaseBranch
	}
	if result.WorkBranch != "" {
		out["work_branch"] = result.WorkBranch
	}
	if result.Lease != nil {
		out["lease_id"] = result.Lease.ID
		out["provider"] = result.Lease.Provider
	}
	return out
}
