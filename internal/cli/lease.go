package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
)

func hasLeases(c Connection) bool {
	return c.Descriptor.Capabilities != nil && supports(c, "execution_leases")
}
func grantEpoch(run *agentcore.AgentRun) int64 {
	switch n := run.Input.Metadata["cli_epoch"].(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}
func leaseRequest(ctx context.Context, home, name string, c Connection, hostID, action string, body any) (ExecutionGrant, error) {
	var e ExecutionGrant
	token, err := accessToken(ctx, home, name, c)
	if err != nil {
		return e, err
	}
	endpoint := c.Descriptor.APIBaseURL + "/runs/" + url.PathEscape(hostID) + "/" + action
	method := "GET"
	var data *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return e, err
		}
		data = bytes.NewReader(raw)
		method = "POST"
	}
	if data == nil {
		err = readJSON(ctx, method, endpoint, token, nil, &e)
	} else {
		err = readJSON(ctx, method, endpoint, token, data, &e)
	}
	return e, err
}
func (s *Session) prepareLease(ctx context.Context, run *agentcore.AgentRun) error {
	name, _ := run.Input.Metadata["cli_connection"].(string)
	if name == "" {
		return nil
	}
	c, err := loadConnection(s.Home, name)
	if err != nil {
		return err
	}
	if !hasLeases(c) {
		return nil
	}
	hostID, _ := run.Input.Metadata["cli_host_run_id"].(string)
	e, err := leaseRequest(ctx, s.Home, name, c, hostID, "execution", nil)
	if err != nil {
		return err
	}
	if !e.LeaseExpiresAt.After(time.Now().Add(time.Minute)) {
		e, err = leaseRequest(ctx, s.Home, name, c, hostID, "renew", map[string]any{"epoch": e.Epoch})
		if err != nil {
			return err
		}
	}
	if e.LocalRunID != "" && e.LocalRunID != run.ID {
		return fmt.Errorf("host grant is bound to another local run")
	}
	e, err = leaseRequest(ctx, s.Home, name, c, hostID, "bind", map[string]any{"epoch": e.Epoch, "local_run_id": run.ID})
	if err != nil {
		return err
	}
	run.Input.Metadata["cli_epoch"] = e.Epoch
	return s.Store.UpdateRun(ctx, run)
}
func (s *Session) maintainLease(ctx context.Context, run *agentcore.AgentRun, cancel context.CancelFunc) func() {
	name, _ := run.Input.Metadata["cli_connection"].(string)
	if name == "" || grantEpoch(run) == 0 {
		return func() {}
	}
	leaseCtx, stopLease := context.WithCancel(ctx)
	ctx = leaseCtx
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				c, err := loadConnection(s.Home, name)
				if err == nil {
					hostID, _ := run.Input.Metadata["cli_host_run_id"].(string)
					var e ExecutionGrant
					e, err = leaseRequest(ctx, s.Home, name, c, hostID, "renew", map[string]any{"epoch": grantEpoch(run)})
					if err == nil && e.Epoch != grantEpoch(run) {
						err = fmt.Errorf("execution lease changed; resume with a fresh grant")
					}
				}
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					eventSink{s.Events}.Emit(ctx, engine.Event{Type: "cli.sync_pending", RunID: run.ID, Data: map[string]any{"message": err.Error()}})
					cancel()
					return
				}
			}
		}
	}()
	return func() { stopLease(); close(stop); <-done }
}
