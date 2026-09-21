package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrDirectPublicationUnsupported reports a workspace provider that cannot
// refresh credentials for a direct push. Callers fall back to publishing with
// the credentials already present in the prepared checkout.
var ErrDirectPublicationUnsupported = errors.New("repository provider does not support direct publication")

// PushWorkspace refreshes host-owned credentials for a directly authorized push.
func (p RepositoryProvider) PushWorkspace(ctx context.Context, req PrepareRequest, root, message string) (json.RawMessage, error) {
	spec, err := p.resolveSpec(ctx, req)
	if err != nil {
		return nil, err
	}
	branch, err := gitOutput(ctx, root, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	spec.WorkBranch = strings.TrimSpace(string(branch))
	if spec.WorkBranch == "" || spec.WorkBranch == spec.BaseBranch || spec.WorkBranch == "HEAD" {
		return nil, fmt.Errorf("create a work branch before publishing")
	}
	if spec.Metadata == nil {
		spec.Metadata = map[string]any{}
	}
	spec.Metadata["commit_message"] = message
	return commitAndPushRepositoryChanges(ctx, root, spec)
}
