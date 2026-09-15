package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/id"
)

// ExecutionGrant is a host-issued, revocable identity for one local execution.
// It grants no authority without the connection's authenticated user token.
type ExecutionGrant struct {
	ID             string    `json:"id"`
	RunID          string    `json:"run_id"`
	Epoch          int64     `json:"epoch"`
	LocalRunID     string    `json:"local_run_id,omitempty"`
	PolicyHash     string    `json:"policy_hash"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

func supports(c Connection, capability string) bool {
	return c.Descriptor.Capabilities == nil || slices.Contains(c.Descriptor.Capabilities, capability)
}
func admissionCommand(ctx context.Context, home string, args []string, out, errout io.Writer) error {
	if args[0] == "executions" {
		return executionCommand(ctx, home, args[1:], out)
	}
	fs := flag.NewFlagSet("admit", flag.ContinueOnError)
	fs.SetOutput(errout)
	var o Options
	fs.StringVar(&o.Connection, "connection", "", "host connection")
	fs.StringVar(&o.Agent, "agent", "", "host agent")
	fs.StringVar(&o.Target, "target", "", "opaque target")
	fs.StringVar(&o.RequestID, "request-id", "", "stable admission request ID")
	fs.BoolVar(&o.Review, "review", false, "review policy")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	o.Prompt = strings.Join(fs.Args(), " ")
	if o.Connection == "" {
		return errors.New("--connection is required")
	}
	if o.RequestID == "" {
		o.RequestID = id.New("cli_request")
	}
	c, err := loadConnection(home, o.Connection)
	if err != nil {
		return err
	}
	if !supports(c, "admission") {
		return errors.New("host does not support local admission")
	}
	if len(o.RequestID) < 8 || !profileName.MatchString(o.RequestID) {
		return errors.New("request ID must contain letters, numbers, underscores or hyphens (8–64 characters)")
	}
	pending := filepath.Join(home, "admissions", o.Connection, o.RequestID+".request.json")
	request := map[string]any{"request_id": o.RequestID, "agent_id": o.Agent, "target": o.Target, "instructions": o.Prompt, "review": o.Review}
	if data, readErr := os.ReadFile(pending); readErr == nil {
		var existing map[string]any
		if err := json.Unmarshal(data, &existing); err != nil {
			return err
		}
		a, _ := json.Marshal(existing)
		b, _ := json.Marshal(request)
		if !bytes.Equal(a, b) {
			return errors.New("request ID already belongs to a different admission; use the original arguments or a new request ID")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err = saveJSON(pending, request); err != nil {
		return err
	}
	fmt.Fprintln(errout, "Admission request:", o.RequestID)
	a, err := admit(ctx, home, o)
	if err != nil {
		return err
	}
	if err = saveJSON(filepath.Join(home, "admissions", o.Connection, o.RequestID+".json"), a); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(a)
}
func executionCommand(ctx context.Context, home string, args []string, out io.Writer) error {
	if len(args) < 3 {
		return errors.New("usage: executions show|bind|renew|revoke CONNECTION HOST_RUN_ID [--epoch N] [--local-run-id ID]")
	}
	action, name, runID := args[0], args[1], args[2]
	if !slices.Contains([]string{"show", "bind", "renew", "revoke"}, action) {
		return errors.New("unknown execution operation")
	}
	fs := flag.NewFlagSet("executions", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var epoch int64
	var localID string
	fs.Int64Var(&epoch, "epoch", 0, "execution epoch")
	fs.StringVar(&localID, "local-run-id", "", "local runtime run ID")
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected execution arguments")
	}
	if (action == "bind" || action == "renew") && epoch < 1 {
		return errors.New("--epoch must identify the current execution version")
	}
	if action == "bind" && localID == "" {
		return errors.New("--local-run-id is required")
	}
	c, err := loadConnection(home, name)
	if err != nil {
		return err
	}
	if !supports(c, "execution_leases") {
		return errors.New("host does not support execution leases")
	}
	token, err := accessToken(ctx, home, name, c)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(c.Descriptor.APIBaseURL, "/") + "/runs/" + url.PathEscape(runID) + "/"
	var result ExecutionGrant
	if action == "show" {
		err = readJSON(ctx, "GET", endpoint+"execution", token, nil, &result)
	} else {
		body, e := json.Marshal(map[string]any{"epoch": epoch, "local_run_id": localID})
		if e != nil {
			return e
		}
		var dst any = &result
		if action == "revoke" {
			dst = nil
		}
		err = readJSON(ctx, "POST", endpoint+action, token, bytes.NewReader(body), dst)
	}
	if err != nil {
		return err
	}
	if action == "revoke" {
		return json.NewEncoder(out).Encode(map[string]string{"run_id": runID, "status": "revoked"})
	}
	return json.NewEncoder(out).Encode(result)
}
