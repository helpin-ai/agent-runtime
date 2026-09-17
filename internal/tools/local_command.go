package tools

import (
	"context"
	"io"
)

type localCommandKey struct{}
type LocalCommandOptions struct {
	// Env is an explicitly selected local environment. Nil preserves server policy.
	Env []string
	// Output receives command chunks; it must be safe for concurrent writes.
	Output io.Writer
}

// WithLocalCommandOptions is a trusted embedding hook, never model input.
func WithLocalCommandOptions(ctx context.Context, options LocalCommandOptions) context.Context {
	if options.Env != nil {
		options.Env = append(make([]string, 0, len(options.Env)), options.Env...)
	}
	return context.WithValue(ctx, localCommandKey{}, options)
}
