# @agent-runtime/react

Native React components and client helpers for embedding agent runs inside host
apps. This is source ESM: import it into the host app and let the host bundler
compile it.

```js
import { AgentRunPanel, createAgentRuntimeClient } from '@agent-runtime/react';

const client = createAgentRuntimeClient({
  baseUrl: '/agent-runtime',
});

<AgentRunPanel appId="host_app" runId={runId} client={client} />;
```

The panel loads the run transcript, interactions, tool calls, artifacts,
persisted event timeline, and Temporal execution metadata in parallel. Temporal
inspection failures are shown inline without hiding the rest of the run.

Host apps still own user auth. In production, proxy these calls through the app
backend or inject a service token only in trusted internal UI surfaces.
