-- Run against the runtime database with psql -X -v ON_ERROR_STOP=1 -f ...
-- No credential payloads or conversation content are selected.
BEGIN READ ONLY;
SET LOCAL statement_timeout = '30s';
SELECT r.runtime_kind, r.invocation_mode, r.status, count(*) AS runs_30d
FROM agent_runs r WHERE r.created_at >= now() - interval '30 days'
GROUP BY 1,2,3 ORDER BY 1,2,3;

-- Every row requires a recorded drain/fail disposition before old pollers stop.
SELECT app_id, id AS runtime_run_id, host_run_id, agent_id, runtime_kind,
       status, pause_reason, approval_state, created_at,
       'agent-run-' || id AS workflow_id
FROM agent_runs
WHERE runtime_kind IN ('codex','opencode')
  AND status NOT IN ('completed','failed','cancelled')
ORDER BY created_at,id;

SELECT app_id,id,runtime_kind FROM agents
WHERE runtime_kind IN ('codex','opencode') ORDER BY app_id,id;
-- Stored connections only; the table has no verified-active/expiry field.
SELECT count(*) AS stored_codex_auth_rows FROM codex_auth_tokens;
SELECT app_id,tenant_id,provider,auth_mode,updated_at FROM codex_auth_tokens
ORDER BY app_id,tenant_id;
COMMIT;
