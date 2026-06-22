# @agent-runtime/react

Native React components and client helpers for embedding agent runs inside host
apps. This is source ESM: import it into the host app and let the host bundler
compile it.

```js
import { AgentRunPanel, createAgentRuntimeClient } from '@agent-runtime/react';

const client = createAgentRuntimeClient({
  baseUrl: '/agent-runtime',
});

<AgentRunPanel appId="contentpen" runId={runId} client={client} />;
```

Host apps still own user auth. In production, proxy these calls through the app
backend or inject a service token only in trusted internal UI surfaces.
