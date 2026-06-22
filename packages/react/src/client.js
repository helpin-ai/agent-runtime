export function createAgentRuntimeClient(options) {
  const baseUrl = String(options?.baseUrl ?? '').replace(/\/+$/, '');
  if (!baseUrl) {
    throw new Error('baseUrl is required');
  }
  const fetchImpl = options?.fetch ?? globalThis.fetch;
  if (!fetchImpl) {
    throw new Error('fetch is not available');
  }
  const token = options?.token ?? '';

  async function request(path, requestOptions = {}) {
    const headers = new Headers(requestOptions.headers ?? {});
    if (token && !headers.has('Authorization')) {
      headers.set('Authorization', `Bearer ${token}`);
    }
    if (requestOptions.body && !headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json');
    }
    const response = await fetchImpl(`${baseUrl}${path}`, {
      ...requestOptions,
      headers,
    });
    const text = await response.text();
    const data = text ? JSON.parse(text) : null;
    if (!response.ok) {
      const message = data?.error ?? data?.message ?? `Agent runtime request failed with ${response.status}`;
      throw new Error(message);
    }
    return data;
  }

  function runPath(appId, runId, suffix = '') {
    const query = `app_id=${encodeURIComponent(appId)}`;
    return `/internal/runs/${encodeURIComponent(runId)}${suffix}?${query}`;
  }

  return {
    getRun: (appId, runId) => request(runPath(appId, runId)),
    listMessages: (appId, runId) => request(runPath(appId, runId, '/messages')),
    listArtifacts: (appId, runId) => request(runPath(appId, runId, '/artifacts')),
    listInteractions: (appId, runId) => request(runPath(appId, runId, '/interactions')),
    listToolCalls: (appId, runId) => request(runPath(appId, runId, '/tool-calls')),
    sendMessage: (appId, runId, content) => request(runPath(appId, runId, '/messages'), {
      method: 'POST',
      body: JSON.stringify({ role: 'user', content }),
    }),
    resumeRun: (appId, runId, payload) => request(runPath(appId, runId, '/resume'), {
      method: 'POST',
      body: JSON.stringify(payload ?? {}),
    }),
    approveRun: (appId, runId) => request(runPath(appId, runId, '/approve'), {
      method: 'POST',
      body: JSON.stringify({}),
    }),
    requestChanges: (appId, runId, content) => request(runPath(appId, runId, '/request-changes'), {
      method: 'POST',
      body: JSON.stringify({ content }),
    }),
    cancelRun: (appId, runId) => request(runPath(appId, runId, '/cancel'), {
      method: 'POST',
      body: JSON.stringify({}),
    }),
  };
}
