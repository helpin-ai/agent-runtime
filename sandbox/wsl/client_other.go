//go:build !windows

package wsl

import (
	"context"
	"encoding/json"
	"errors"
)

func (c Config) Run(context.Context, string, any) (json.RawMessage, error) {
	return nil, errors.New("WSL commands are only available on Windows")
}
func (c Config) Probe(context.Context) (ProbeResult, error) {
	return ProbeResult{}, errors.New("WSL commands are only available on Windows")
}
func NativePath(string) (string, error) {
	return "", errors.New("WSL path mapping is only available on Windows")
}
