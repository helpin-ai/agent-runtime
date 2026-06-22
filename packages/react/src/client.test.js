import assert from 'node:assert/strict';
import test from 'node:test';
import { createAgentRuntimeClient } from './client.js';

test('client sends app scoped run requests with bearer token', async () => {
  const calls = [];
  const client = createAgentRuntimeClient({
    baseUrl: 'https://runtime.internal/',
    token: 'service-token',
    fetch: async (url, options) => {
      calls.push({ url, options });
      return new Response(JSON.stringify({ id: 'run-1' }), { status: 200 });
    },
  });

  const run = await client.getRun('app-a', 'run-1');

  assert.equal(run.id, 'run-1');
  assert.equal(calls[0].url, 'https://runtime.internal/internal/runs/run-1?app_id=app-a');
  assert.equal(calls[0].options.headers.get('Authorization'), 'Bearer service-token');
});

test('client lists durable tool calls', async () => {
  const calls = [];
  const client = createAgentRuntimeClient({
    baseUrl: 'https://runtime.internal',
    fetch: async (url, options) => {
      calls.push({ url, options });
      return new Response(JSON.stringify([{ id: 'toolcall-1', tool_name: 'run_command' }]), { status: 200 });
    },
  });

  const toolCalls = await client.listToolCalls('app-a', 'run-1');

  assert.equal(toolCalls[0].tool_name, 'run_command');
  assert.equal(calls[0].url, 'https://runtime.internal/internal/runs/run-1/tool-calls?app_id=app-a');
});

test('client surfaces runtime errors', async () => {
  const client = createAgentRuntimeClient({
    baseUrl: 'https://runtime.internal',
    fetch: async () => new Response(JSON.stringify({ error: 'run not found' }), { status: 404 }),
  });

  await assert.rejects(() => client.getRun('app-a', 'missing'), /run not found/);
});
