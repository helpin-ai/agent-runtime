from __future__ import annotations

from typing import Any, Dict, List, Optional

import httpx

from .models import (
    Agent,
    AgentRun,
    AgentRunArtifact,
    AgentRunInteraction,
    AgentRunMessage,
    CodexAuthState,
    ResumeRunRequest,
    StartRunRequest,
    ToolDefinition,
    ToolResult,
    ToolCall,
)


class AgentRuntimeError(RuntimeError):
    pass


class AgentRuntimeClient:
    def __init__(
        self,
        base_url: str,
        app_id: str,
        service_token: Optional[str] = None,
        client: Optional[httpx.Client] = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.app_id = app_id
        self.service_token = service_token
        self.client = client or httpx.Client(timeout=30.0)

    def close(self) -> None:
        self.client.close()

    def health(self) -> Dict[str, Any]:
        return self._request("GET", "/healthz")

    def create_agent(self, agent: Agent | Dict[str, Any]) -> Agent:
        data = self._request("POST", "/v1/agents", json=self._dump(agent))
        return Agent(**data)

    def get_agent(self, agent_id: str) -> Agent:
        data = self._request(
            "GET",
            f"/v1/agents/{agent_id}",
            params={"app_id": self.app_id},
        )
        return Agent(**data)

    def list_agents(self) -> List[Agent]:
        data = self._request("GET", "/v1/agents", params={"app_id": self.app_id})
        return [Agent(**item) for item in data]

    def update_agent(self, agent_id: str, agent: Agent | Dict[str, Any]) -> Agent:
        payload = self._dump(agent)
        payload["app_id"] = self.app_id
        payload["id"] = agent_id
        data = self._request(
            "PUT",
            f"/v1/agents/{agent_id}",
            params={"app_id": self.app_id},
            json=payload,
        )
        return Agent(**data)

    def upsert_agent(self, agent: Agent | Dict[str, Any]) -> Agent:
        payload = self._dump(agent)
        agent_id = payload.get("id")
        if not agent_id:
            raise AgentRuntimeError("agent id is required for upsert")
        return self.update_agent(str(agent_id), payload)

    def start_run(self, request: StartRunRequest | Dict[str, Any]) -> AgentRun:
        payload = self._dump(request)
        payload.setdefault("app_id", self.app_id)
        data = self._request("POST", "/v1/runs", json=payload)
        return AgentRun(**data)

    def get_run(self, run_id: str) -> AgentRun:
        data = self._request("GET", f"/v1/runs/{run_id}", params={"app_id": self.app_id})
        return AgentRun(**data)

    def list_runs(self) -> List[AgentRun]:
        data = self._request("GET", "/v1/runs", params={"app_id": self.app_id})
        return [AgentRun(**item) for item in data]

    def list_messages(self, run_id: str) -> List[AgentRunMessage]:
        data = self._request("GET", f"/v1/runs/{run_id}/messages", params={"app_id": self.app_id})
        return [AgentRunMessage(**item) for item in data]

    def send_message(self, run_id: str, content: str, role: str = "user") -> AgentRunMessage:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/messages",
            params={"app_id": self.app_id},
            json={"role": role, "content": content},
        )
        return AgentRunMessage(**data)

    def list_artifacts(self, run_id: str) -> List[AgentRunArtifact]:
        data = self._request("GET", f"/v1/runs/{run_id}/artifacts", params={"app_id": self.app_id})
        return [AgentRunArtifact(**item) for item in data]

    def append_artifact(
        self,
        run_id: str,
        artifact_type: str,
        inline_content: str,
        *,
        format: str = "json",
        storage_mode: str = "inline",
        metadata: Optional[Dict[str, Any]] = None,
    ) -> AgentRunArtifact:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/artifacts",
            params={"app_id": self.app_id},
            json={
                "artifact_type": artifact_type,
                "format": format,
                "storage_mode": storage_mode,
                "inline_content": inline_content,
                "metadata": metadata or {},
            },
        )
        return AgentRunArtifact(**data)

    def list_interactions(self, run_id: str) -> List[AgentRunInteraction]:
        data = self._request("GET", f"/v1/runs/{run_id}/interactions", params={"app_id": self.app_id})
        return [AgentRunInteraction(**item) for item in data]

    def list_tool_calls(self, run_id: str) -> List[ToolCall]:
        data = self._request("GET", f"/v1/runs/{run_id}/tool-calls", params={"app_id": self.app_id})
        return [ToolCall(**item) for item in data]

    def list_run_tools(self, run_id: str) -> List[ToolDefinition]:
        data = self._request("GET", f"/v1/runs/{run_id}/tools", params={"app_id": self.app_id})
        return [ToolDefinition(**item) for item in data.get("tools", [])]

    def call_run_tool(self, run_id: str, tool_name: str, input: Optional[Dict[str, Any]] = None) -> ToolResult:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/tools",
            params={"app_id": self.app_id},
            json={"tool_name": tool_name, "input": input or {}},
        )
        return ToolResult(**data)

    def resume_run(self, run_id: str, request: ResumeRunRequest | Dict[str, Any]) -> AgentRun:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/resume",
            params={"app_id": self.app_id},
            json=self._dump(request),
        )
        return AgentRun(**data)

    def approve_run(self, run_id: str) -> AgentRun:
        data = self._request("POST", f"/v1/runs/{run_id}/approve", params={"app_id": self.app_id}, json={})
        return AgentRun(**data)

    def request_changes(self, run_id: str, content: str) -> AgentRun:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/request-changes",
            params={"app_id": self.app_id},
            json={"content": content},
        )
        return AgentRun(**data)

    def cancel_run(self, run_id: str) -> AgentRun:
        data = self._request("POST", f"/v1/runs/{run_id}/cancel", params={"app_id": self.app_id}, json={})
        return AgentRun(**data)

    def start_codex_device_code_auth(self, run_id: str) -> CodexAuthState:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/codex-auth/device-code/start",
            params={"app_id": self.app_id},
            json={},
        )
        return CodexAuthState(**data)

    def cancel_codex_device_code_auth(self, run_id: str) -> CodexAuthState:
        data = self._request(
            "POST",
            f"/v1/runs/{run_id}/codex-auth/device-code/cancel",
            params={"app_id": self.app_id},
            json={},
        )
        return CodexAuthState(**data)

    def _request(self, method: str, path: str, **kwargs: Any) -> Any:
        headers = dict(kwargs.pop("headers", {}) or {})
        if self.service_token and "Authorization" not in headers:
            headers["Authorization"] = f"Bearer {self.service_token}"
        response = self.client.request(method, f"{self.base_url}{path}", headers=headers, **kwargs)
        try:
            data = response.json() if response.content else None
        except ValueError:
            data = None
        if response.status_code >= 300:
            message = None
            if isinstance(data, dict):
                message = data.get("error") or data.get("message")
            raise AgentRuntimeError(message or f"agent-runtime request failed with {response.status_code}")
        return data

    def _dump(self, value: Any) -> Dict[str, Any]:
        if isinstance(value, dict):
            return dict(value)
        if hasattr(value, "model_dump"):
            return value.model_dump(exclude_none=True)
        return value.dict(exclude_none=True)
