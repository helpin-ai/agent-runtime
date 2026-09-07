package agentcore

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrNativeStateConflict prevents two executors from overwriting a checkpoint.
var ErrNativeStateConflict = errors.New("native execution checkpoint changed")

// NativeState is private runtime state, independent of the host's run projection.
type NativeState struct {
	AppID   string
	RunID   string
	Version int64
	Payload json.RawMessage
}

// NativeStateStore atomically checkpoints active context and archives new records.
// It is optional so existing Store implementations remain source compatible.
type NativeStateStore interface {
	LoadNativeState(context.Context, string, string) (*NativeState, error)
	SaveNativeState(context.Context, *NativeState, []json.RawMessage) error
}
