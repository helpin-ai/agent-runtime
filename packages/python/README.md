# agent-runtime Python SDK

Thin Python client and adapter helpers for the service-first Agent Runtime.

```python
from agent_runtime import AgentRuntimeClient

client = AgentRuntimeClient(
    base_url="https://agent-runtime.internal",
    app_id="contentpen",
    service_token="service-token",
)

run = client.start_run({
    "agent_id": "agent_123",
    "target": {"type": "article", "id": "article_123"},
    "instructions": "Review this article.",
})
```

Target context is HTTP because it is part of run lifecycle and identity.

```python
from agent_runtime import TargetContextResponse, create_fastapi_target_context_router

async def resolve_context(request):
    return TargetContextResponse(
        target=request.target,
        summary=f"Fresh context for {request.target.type}/{request.target.id}",
        data={},
    )

app.include_router(create_fastapi_target_context_router(resolve_context, token="service-token"))
```

Tools should be exposed through MCP and configured in `AGENT_RUNTIME_APP_CONFIG`.
