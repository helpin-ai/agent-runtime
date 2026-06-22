export const ACTIVE_RUN_STATUSES = new Set(['queued', 'running', 'paused']);

export const STATUS_META = {
  queued: { label: 'Queued', tone: 'muted' },
  running: { label: 'Running', tone: 'active' },
  paused: { label: 'Paused', tone: 'attention' },
  awaiting_input: { label: 'Awaiting input', tone: 'attention' },
  awaiting_approval: { label: 'Awaiting approval', tone: 'attention' },
  awaiting_auth: { label: 'Awaiting sign-in', tone: 'attention' },
  completed: { label: 'Completed', tone: 'success' },
  failed: { label: 'Failed', tone: 'danger' },
  cancelled: { label: 'Cancelled', tone: 'muted' },
};

export const ARTIFACT_TYPE_LABELS = {
  conversation_log: 'Conversation Log',
  tool_log: 'Tool Log',
  diff: 'Diff',
  test_report: 'Test Report',
  pr_metadata: 'PR Metadata',
  agent_summary: 'Agent Summary',
  file_bundle: 'File Bundle',
  handoff_note: 'Handoff Note',
  run_plan: 'Execution Plan',
  review_findings: 'Review Findings',
  review_decision: 'Review Decision',
};

export function getAgentRunPauseReason(run) {
  if (!run) return 'none';
  if (run.pause_reason && run.pause_reason !== 'none') return run.pause_reason;
  if (run.status === 'paused' && run.approval_state === 'pending') return 'human_approval';
  if (run.status === 'paused') return 'human_input';
  return 'none';
}

export function getAgentRunDisplayStatus(run) {
  if (!run) return 'queued';
  if (run.status === 'paused') {
    const pauseReason = getAgentRunPauseReason(run);
    if (pauseReason === 'human_approval') return 'awaiting_approval';
    if (pauseReason === 'authentication') return 'awaiting_auth';
    return 'awaiting_input';
  }
  return run.status;
}

export function isPausedAgentRun(run) {
  const status = getAgentRunDisplayStatus(run);
  return status === 'awaiting_input' || status === 'awaiting_approval' || status === 'awaiting_auth';
}

export function artifactLabel(artifactType) {
  if (!artifactType) return 'Artifact';
  return ARTIFACT_TYPE_LABELS[artifactType] ?? artifactType.replaceAll('_', ' ');
}
