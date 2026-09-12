-- Forward runtime migration; apply after both inventories and workflow disposal.
BEGIN;
LOCK TABLE agent_runs IN SHARE ROW EXCLUSIVE MODE;
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM agent_runs WHERE runtime_kind IN ('codex','opencode')
             AND status NOT IN ('completed','failed','cancelled')) THEN
    RAISE EXCEPTION 'Native cutover blocked: dispose of retired-engine runs and workflows first';
  END IF;
END $$;
UPDATE agents SET runtime_kind='native_sdk', updated_at=now()
WHERE runtime_kind IN ('codex','opencode');
DROP TABLE IF EXISTS codex_auth_tokens;
COMMIT;
