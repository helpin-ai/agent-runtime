package wsl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func (c Config) Probe(ctx context.Context) (ProbeResult, error) {
	var result ProbeResult
	if err := c.Validate(); err != nil {
		return result, err
	}
	if err := os.MkdirAll(c.ProbeRoot, 0700); err != nil {
		return result, err
	}
	root, err := os.MkdirTemp(c.ProbeRoot, ".runtime-wsl-probe-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(root)
	outside, err := os.CreateTemp("", "runtime-wsl-outside-")
	if err != nil {
		return result, err
	}
	defer os.Remove(outside.Name())
	if _, err := outside.WriteString("confinement probe fixture"); err != nil {
		outside.Close()
		return result, err
	}
	if err := outside.Close(); err != nil {
		return result, err
	}
	nativeRoot, err := NativePath(root)
	if err != nil {
		return result, err
	}
	nativeOutside, err := NativePath(outside.Name())
	if err != nil {
		return result, err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return result, err
	}
	executable, err := NativePath(filepath.Join(system, "cmd.exe"))
	if err != nil {
		return result, err
	}
	raw, err := c.Run(ctx, "probe", ProbeRequest{Root: nativeRoot, Outside: nativeOutside, WindowsExecutable: executable})
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(raw, &result)
	return result, err
}
