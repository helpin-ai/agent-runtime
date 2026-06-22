import assert from 'node:assert/strict';
import test from 'node:test';
import {
  artifactLabel,
  getAgentRunDisplayStatus,
  getAgentRunPauseReason,
  isPausedAgentRun,
} from './status.js';

test('maps paused approval run to awaiting approval', () => {
  const run = { status: 'paused', pause_reason: 'none', approval_state: 'pending' };
  assert.equal(getAgentRunPauseReason(run), 'human_approval');
  assert.equal(getAgentRunDisplayStatus(run), 'awaiting_approval');
  assert.equal(isPausedAgentRun(run), true);
});

test('maps authentication pause to awaiting auth', () => {
  const run = { status: 'paused', pause_reason: 'authentication', approval_state: 'not_required' };
  assert.equal(getAgentRunDisplayStatus(run), 'awaiting_auth');
});

test('formats artifact labels', () => {
  assert.equal(artifactLabel('run_plan'), 'Execution Plan');
  assert.equal(artifactLabel('custom_output'), 'custom output');
});
