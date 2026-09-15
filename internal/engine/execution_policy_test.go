package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/store"
)

func TestCodingAdmissionUsesEffectiveTools(t *testing.T) {
	for _, tt := range []struct {
		name             string
		tools, requested []string
		wantCoding       bool
	}{
		{"support mutation", []string{"update_ticket"}, nil, false},
		{"custom shell", []string{"run_command"}, nil, true},
		{"write", []string{"edit_file"}, nil, true},
		{"narrowed to read", []string{"read_files", "apply_patch"}, []string{"read_files"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := New(Config{Store: store.NewMemory()})
			a := &agentcore.Agent{RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: tt.tools}
			err := e.admitTools(context.Background(), a, tt.requested, ExecutionModeDurable)
			if errors.Is(err, ErrCodingUnavailable) != tt.wantCoding {
				t.Fatalf("admission=%v", err)
			}
			if tt.wantCoding {
				e.cfg.CheckCodingAdmission = func(context.Context) error { return nil }
				if err := e.admitTools(context.Background(), a, tt.requested, ExecutionModeDurable); err != nil {
					t.Fatal(err)
				}
				if err := e.executionPolicy(a, &agentcore.AgentRun{RuntimeKind: agentcore.RuntimeNativeSDK}); !errors.Is(err, ErrCodingUnavailable) {
					t.Fatalf("support execution=%v", err)
				}
			}
		})
	}
}

func TestRetiredRunCannotResume(t *testing.T) {
	for _, kind := range []string{"codex", "opencode"} {
		t.Run(kind, func(t *testing.T) {
			s := store.NewMemory()
			r := &agentcore.AgentRun{ID: "old", AppID: "a", RuntimeKind: kind, Status: agentcore.RunStatusPaused}
			if err := s.CreateRun(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			_, err := New(Config{Store: s}).ResumeRun(context.Background(), "a", "old", ResumePayload{Intent: "reply"})
			if !errors.Is(err, ErrRetiredRuntime) {
				t.Fatalf("resume=%v", err)
			}
			got, err := s.GetRun(context.Background(), "a", "old")
			if err != nil {
				t.Fatal(err)
			}
			if got.RuntimeKind != kind {
				t.Fatal("rewrote historical engine")
			}
		})
	}
}

func TestCodingContinuationRejectsMissingHostPreparedWorkspace(t *testing.T) {
	e := New(Config{Store: store.NewMemory(), CodingWorker: true})
	a := &agentcore.Agent{RuntimeKind: agentcore.RuntimeNativeSDK, AllowedTools: []string{"write_file"}, ExecutionConfig: []byte(`{"workspace":{"mode":"host_prepared"}}`)}
	r := &agentcore.AgentRun{RuntimeKind: agentcore.RuntimeNativeSDK, WorkspaceLease: &agentcore.WorkspaceLease{RootPath: filepath.Join(t.TempDir(), "missing")}}
	_, err := e.ensureWorkspace(context.Background(), a, r, nil)
	if err == nil || !strings.Contains(err.Error(), "workspace for this run is unavailable") {
		t.Fatalf("missing coding workspace error=%v", err)
	}
}
