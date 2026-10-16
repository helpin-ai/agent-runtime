package wsl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

func (c Config) Validate() error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`).MatchString(c.Distribution) {
		return errors.New("a valid WSL distribution is required")
	}
	if !strings.HasPrefix(c.Helper, "/") || strings.ContainsAny(c.Helper, "\x00\r\n") {
		return errors.New("an absolute Linux sandbox helper path is required")
	}
	if !filepath.IsAbs(c.ProbeRoot) || filepath.Dir(filepath.Clean(c.ProbeRoot)) == filepath.Clean(c.ProbeRoot) {
		return errors.New("a dedicated native WSL probe workspace is required")
	}
	return nil
}

func NativePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(absolute) {
		return "", errors.New("WSL commands require a workspace on a local Windows drive")
	}
	return "/mnt/" + strings.ToLower(volume[:1]) + filepath.ToSlash(absolute[len(volume):]), nil
}

// Run keeps stdin open as a lifetime lease. EOF cancels the Linux process tree.
func (c Config) Run(ctx context.Context, mode string, request any) (json.RawMessage, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if mode != "execute" && mode != "probe" {
		return nil, errors.New("invalid WSL helper mode")
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(request)
	if err != nil || len(input)+1 > MaxMessageBytes {
		return nil, errors.New("WSL request exceeds its limit")
	}
	command := exec.CommandContext(ctx, filepath.Join(system, "wsl.exe"), "--distribution", c.Distribution, "--exec", c.Helper, mode)
	command.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "WINDIR=" + os.Getenv("WINDIR"), "USERPROFILE=" + os.Getenv("USERPROFILE"), "LOCALAPPDATA=" + os.Getenv("LOCALAPPDATA"), "PATH=" + system}
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	pipe, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer pipe.Close()
	command.Cancel = pipe.Close
	command.WaitDelay = 5 * time.Second
	var output, stderr boundedBuffer
	command.Stdout, command.Stderr = &output, &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	writeDone := make(chan error, 1)
	go func() { _, err := pipe.Write(append(input, '\n')); writeDone <- err }()
	err = command.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if writeErr := <-writeDone; writeErr != nil {
		return nil, fmt.Errorf("write WSL request: %w", writeErr)
	}
	if err != nil {
		return nil, fmt.Errorf("WSL sandbox helper: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var response Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("invalid WSL sandbox response: %w", err)
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}
