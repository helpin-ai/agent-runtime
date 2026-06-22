import { useCallback, useEffect, useMemo, useState } from 'react';
import { createAgentRuntimeClient } from './client.js';

export function useAgentRun(options) {
  const {
    appId,
    runId,
    client,
    baseUrl,
    token,
    pollMs = 3000,
    enabled = true,
  } = options ?? {};
  const runtimeClient = useMemo(() => {
    if (client) return client;
    if (!baseUrl) return null;
    return createAgentRuntimeClient({ baseUrl, token });
  }, [baseUrl, client, token]);
  const [state, setState] = useState({
    run: null,
    messages: [],
    artifacts: [],
    interactions: [],
    toolCalls: [],
    loading: Boolean(enabled && appId && runId),
    error: null,
  });

  const refresh = useCallback(async () => {
    if (!enabled || !runtimeClient || !appId || !runId) return null;
    setState((current) => ({ ...current, loading: true, error: null }));
    try {
      const [run, messages, artifacts, interactions, toolCalls] = await Promise.all([
        runtimeClient.getRun(appId, runId),
        runtimeClient.listMessages(appId, runId),
        runtimeClient.listArtifacts(appId, runId),
        runtimeClient.listInteractions(appId, runId),
        runtimeClient.listToolCalls ? runtimeClient.listToolCalls(appId, runId) : Promise.resolve([]),
      ]);
      setState({ run, messages, artifacts, interactions, toolCalls, loading: false, error: null });
      return run;
    } catch (error) {
      setState((current) => ({ ...current, loading: false, error }));
      return null;
    }
  }, [appId, enabled, runId, runtimeClient]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    if (!enabled || !pollMs || !runtimeClient || !appId || !runId) return undefined;
    const id = setInterval(() => {
      void refresh();
    }, pollMs);
    return () => clearInterval(id);
  }, [appId, enabled, pollMs, refresh, runId, runtimeClient]);

  const actions = useMemo(() => ({
    sendMessage: async (content) => {
      await runtimeClient.sendMessage(appId, runId, content);
      return refresh();
    },
    resume: async (payload) => {
      await runtimeClient.resumeRun(appId, runId, payload);
      return refresh();
    },
    approve: async () => {
      await runtimeClient.approveRun(appId, runId);
      return refresh();
    },
    requestChanges: async (content) => {
      await runtimeClient.requestChanges(appId, runId, content);
      return refresh();
    },
    cancel: async () => {
      await runtimeClient.cancelRun(appId, runId);
      return refresh();
    },
  }), [appId, refresh, runId, runtimeClient]);

  return { ...state, refresh, actions };
}
