package store

import (
	"context"
	"fmt"
)

func (s *SQL) MigratePostgres(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sql store is not configured")
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			app_id TEXT NOT NULL,
			name TEXT NOT NULL,
			runtime_kind TEXT NOT NULL,
			provider TEXT,
			model TEXT,
			system_prompt TEXT,
			skills JSONB NOT NULL DEFAULT '[]'::jsonb,
			allowed_tools JSONB NOT NULL DEFAULT '[]'::jsonb,
			allowed_targets JSONB NOT NULL DEFAULT '[]'::jsonb,
			approval_mode TEXT NOT NULL,
			default_invocation_mode TEXT NOT NULL,
			execution_config JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_app_id ON agents(app_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_app_created ON agents(app_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS agent_runs (
			id TEXT PRIMARY KEY,
			app_id TEXT NOT NULL,
			host_run_id TEXT,
			agent_id TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT NOT NULL,
			target_display JSONB NOT NULL DEFAULT '{}'::jsonb,
			target_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			runtime_kind TEXT NOT NULL,
			execution_mode TEXT NOT NULL,
			invocation_mode TEXT NOT NULL,
			external_actor_id TEXT,
			status TEXT NOT NULL,
			pause_reason TEXT NOT NULL,
			approval_state TEXT NOT NULL,
			input JSONB NOT NULL DEFAULT '{}'::jsonb,
			output_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
			workspace_lease JSONB,
			error_message TEXT,
			started_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS host_run_id TEXT`,
		`ALTER TABLE agent_runs ADD COLUMN IF NOT EXISTS workspace_lease JSONB`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_app_id ON agent_runs(app_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_host_run_id ON agent_runs(app_id, host_run_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_app_host_run_id ON agent_runs(app_id, host_run_id) WHERE host_run_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_runs_app_created ON agent_runs(app_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_app_target ON agent_runs(app_id, target_type, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_agent_id ON agent_runs(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_status ON agent_runs(status)`,
		`CREATE TABLE IF NOT EXISTS agent_run_messages (
			id TEXT PRIMARY KEY,
			app_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			runtime_message_id TEXT,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			message_type TEXT NOT NULL,
			content_blocks JSONB NOT NULL DEFAULT '{}'::jsonb,
			tool_invocations JSONB NOT NULL DEFAULT '{}'::jsonb,
			sequence_no INTEGER NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`ALTER TABLE agent_run_messages ADD COLUMN IF NOT EXISTS runtime_message_id TEXT`,
		`CREATE INDEX IF NOT EXISTS idx_messages_runtime_message_id ON agent_run_messages(runtime_message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_run_seq ON agent_run_messages(app_id, run_id, sequence_no)`,
		`CREATE TABLE IF NOT EXISTS agent_run_artifacts (
			id TEXT PRIMARY KEY,
			app_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			artifact_type TEXT NOT NULL,
			format TEXT NOT NULL,
			storage_mode TEXT NOT NULL,
			inline_content TEXT,
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			sequence_no INTEGER NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_run_seq ON agent_run_artifacts(app_id, run_id, sequence_no)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_type ON agent_run_artifacts(artifact_type)`,
		`CREATE TABLE IF NOT EXISTS agent_run_interactions (
			id TEXT PRIMARY KEY,
			app_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			runtime_kind TEXT NOT NULL,
			interaction_kind TEXT NOT NULL,
			status TEXT NOT NULL,
			title TEXT,
			summary TEXT,
			request_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
			response_payload JSONB,
			resolved_by_external_id TEXT,
			resolved_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_interactions_run_created ON agent_run_interactions(app_id, run_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_interactions_kind ON agent_run_interactions(interaction_kind)`,
		`CREATE INDEX IF NOT EXISTS idx_interactions_status ON agent_run_interactions(status)`,
		`CREATE TABLE IF NOT EXISTS agent_run_tool_calls (
			id TEXT PRIMARY KEY,
			app_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			input JSONB NOT NULL DEFAULT '{}'::jsonb,
			output JSONB,
			error TEXT,
			mutating BOOLEAN NOT NULL DEFAULT false,
			approval_required BOOLEAN NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tool_calls_run_created ON agent_run_tool_calls(app_id, run_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_tool_calls_name ON agent_run_tool_calls(tool_name)`,
		`CREATE TABLE IF NOT EXISTS codex_auth_tokens (
			app_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL,
			auth_mode TEXT NOT NULL,
			payload BYTEA NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (app_id, tenant_id, provider, auth_mode)
		)`,
	}
	for _, statement := range statements {
		if err := s.db.WithContext(ctx).Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
