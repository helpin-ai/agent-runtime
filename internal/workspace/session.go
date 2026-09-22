package workspace

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

const StorageModeEnv = "AGENT_RUNTIME_WORKSPACE_STORAGE"
const SessionMetadataKey = "execution_session"
const RecoveryMetadataKey = "workspace_recovery_id"

type sessionContextKey struct{}

func EphemeralWorkspaces() bool { return strings.TrimSpace(os.Getenv(StorageModeEnv)) == "ephemeral" }

func ValidateWorkspaceStorage() error {
	switch strings.TrimSpace(os.Getenv(StorageModeEnv)) {
	case "", "persistent":
		return nil
	case "ephemeral":
		if info, err := os.Stat(WorkspaceRoot()); err != nil || !info.IsDir() {
			return fmt.Errorf("ephemeral workspace root must be an existing local directory")
		}
		return validateLocalCacheFilesystem(WorkspaceRoot())
	default:
		return fmt.Errorf("%s must be persistent or ephemeral", StorageModeEnv)
	}
}

// WithSession is only supplied by session-bound durable activities, never by
// caller metadata. A new session gets a distinct directory even on the same pod.
func WithSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, session)
}

func Session(ctx context.Context) string {
	session, _ := ctx.Value(sessionContextKey{}).(string)
	return session
}

func SessionRoot(ctx context.Context, root string) string {
	if session := Session(ctx); session != "" {
		return filepath.Join(root, ".sessions", fmt.Sprintf("%x", sha256.Sum256([]byte(session))))
	}
	return root
}

func SessionLease(ctx context.Context, lease *agentcore.WorkspaceLease) *agentcore.WorkspaceLease {
	if lease != nil && Session(ctx) != "" {
		NormalizeLease(lease)
		lease.Metadata[SessionMetadataKey] = Session(ctx)
	}
	return lease
}

func LeaseInSession(ctx context.Context, lease *agentcore.WorkspaceLease) bool {
	return lease != nil && (Session(ctx) == "" || lease.Metadata[SessionMetadataKey] == Session(ctx))
}
