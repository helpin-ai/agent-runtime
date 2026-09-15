package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
)

func TestSQLModelCredentialRestartCASAndTerminalCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := SQLConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "runs.sqlite")}
	db, err := OpenSQL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(); err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{AppID: "app", ID: "run", Status: agentcore.RunStatusPaused}
	credential := &agentcore.RunModelCredential{AppID: "app", RunID: "run", Provider: "openai", Kind: "api_key", EncryptedCredential: []byte("sealed"), Version: 1}
	if err = db.CreateRunWithModelCredential(ctx, run, nil, credential); err != nil {
		t.Fatal(err)
	}
	// A fresh store instance must see the same encrypted credential after a restart.
	restarted, err := OpenSQL(cfg)
	if err != nil {
		t.Fatal(err)
	}
	found, err := restarted.GetRunModelCredential(ctx, "app", "run")
	if err != nil || found == nil || string(found.EncryptedCredential) != "sealed" {
		t.Fatalf("restart: %v %v", found, err)
	}
	other, err := restarted.GetRunModelCredential(ctx, "other", "run")
	if err != nil || other != nil {
		t.Fatal("cross-app lookup")
	}
	if ok, err := restarted.ReplaceRunModelCredential(ctx, credential, 1); err != nil || !ok {
		t.Fatalf("rotate: %v %v", ok, err)
	}
	if ok, err := restarted.ReplaceRunModelCredential(ctx, credential, 1); err != nil || ok {
		t.Fatalf("stale rotation: %v %v", ok, err)
	}
	run.Status = agentcore.RunStatusCompleted
	if err = restarted.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err = restarted.ClearRunModelCredential(ctx, "app", "run"); err != nil {
		t.Fatal(err)
	}
	if ok, err := restarted.ReplaceRunModelCredential(ctx, credential, 3); err != nil || ok {
		t.Fatal("terminal credential resurrected")
	}
	found, err = restarted.GetRunModelCredential(ctx, "app", "run")
	if err != nil || !found.Revoked || len(found.EncryptedCredential) != 0 {
		t.Fatal("secret not cleared")
	}
}

func TestSQLRunAndCredentialInsertAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := newTestSQLStore(t)
	credential := &agentcore.RunModelCredential{AppID: "app", RunID: "collision", Version: 1}
	if err := db.db.Create(credential).Error; err != nil {
		t.Fatal(err)
	}
	run := &agentcore.AgentRun{AppID: "app", ID: "collision"}
	if err := db.CreateRunWithModelCredential(ctx, run, nil, credential); err == nil {
		t.Fatal("expected collision")
	}
	found, err := db.GetRun(ctx, "app", "collision")
	if err != nil || found != nil {
		t.Fatal("run committed without credential")
	}
}
