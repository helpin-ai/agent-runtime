import json
import unittest

import httpx

from agent_runtime import (
    Agent,
    AgentRuntimeClient,
    AgentRuntimeError,
    AppConfig,
    CommandExecutionRequest,
    MCPProviderConfig,
    PrepareWorkspaceRequest,
    RepositoryWorkspaceSpec,
    SkillLookupRequest,
    TargetContextRequest,
    TargetRef,
    ToolCallRequest,
    ToolResult,
    WorkspaceSkill,
    create_fastapi_mcp_provider_router,
    verify_bearer_token,
)


def run_payload(run_id="run-1"):
    return {
        "id": run_id,
        "app_id": "app-a",
        "agent_id": "agent-1",
        "target": {"type": "ticket", "id": "T-1", "metadata": {}},
        "runtime_kind": "native_sdk",
        "execution_mode": "lightweight",
        "invocation_mode": "autonomous",
        "status": "queued",
        "pause_reason": "none",
        "approval_state": "not_required",
        "input": {"allowed_tools": [], "trigger": {}, "metadata": {}},
        "output_summary": {},
    }


def agent_payload(agent_id="agent-1", model="gpt-4.1-mini"):
    return {
        "id": agent_id,
        "app_id": "app-a",
        "name": "Agent",
        "runtime_kind": "native_sdk",
        "provider": "openai",
        "model": model,
        "skills": [],
        "allowed_tools": ["update_plan"],
        "allowed_targets": ["message_generation_task"],
        "approval_mode": "never",
        "default_invocation_mode": "interactive",
        "execution_config": {},
    }


class ClientTests(unittest.TestCase):
    def test_health(self):
        def handler(request):
            self.assertEqual(request.url.path, "/healthz")
            return httpx.Response(200, json={"status": "ok"})

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        self.assertEqual(client.health(), {"status": "ok"})

    def test_client_sends_v1_requests_with_auth(self):
        calls = []

        def handler(request):
            calls.append(request)
            self.assertEqual(request.headers["authorization"], "Bearer secret")
            self.assertEqual(request.url.path, "/v1/runs")
            self.assertEqual(request.url.params["app_id"], "app-a")
            return httpx.Response(200, json=[run_payload()])

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            service_token="secret",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )

        runs = client.list_runs()

        self.assertEqual(len(runs), 1)
        self.assertEqual(runs[0].id, "run-1")
        self.assertEqual(len(calls), 1)

    def test_start_run_defaults_app_id(self):
        def handler(request):
            body = json.loads(request.content)
            self.assertEqual(body["app_id"], "app-a")
            return httpx.Response(202, json=run_payload())

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        run = client.start_run({"agent_id": "agent-1", "target": {"type": "ticket", "id": "T-1"}})
        self.assertEqual(run.id, "run-1")

    def test_agent_get_update_and_upsert(self):
        calls = []

        def handler(request):
            calls.append(request)
            if request.method == "GET":
                self.assertEqual(request.url.path, "/v1/agents/agent-1")
                self.assertEqual(request.url.params["app_id"], "app-a")
                return httpx.Response(200, json=agent_payload())
            body = json.loads(request.content)
            self.assertEqual(request.method, "PUT")
            self.assertEqual(request.url.path, "/v1/agents/agent-1")
            self.assertEqual(request.url.params["app_id"], "app-a")
            self.assertEqual(body["id"], "agent-1")
            self.assertEqual(body["app_id"], "app-a")
            return httpx.Response(200, json=agent_payload(model=body["model"]))

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )

        self.assertEqual(client.get_agent("agent-1").id, "agent-1")
        self.assertEqual(client.update_agent("agent-1", {"name": "Agent", "model": "gpt-5-mini"}).model, "gpt-5-mini")
        self.assertEqual(client.upsert_agent({"id": "agent-1", "name": "Agent", "model": "gpt-5-mini"}).model, "gpt-5-mini")
        self.assertEqual(len(calls), 3)

    def test_list_tool_calls(self):
        def handler(request):
            self.assertEqual(request.url.path, "/v1/runs/run-1/tool-calls")
            self.assertEqual(request.url.params["app_id"], "app-a")
            return httpx.Response(200, json=[{
                "id": "toolcall-1",
                "app_id": "app-a",
                "run_id": "run-1",
                "tool_name": "run_command",
                "input": {"command": "go test ./..."},
                "output": {"summary": "ok"},
                "mutating": True,
                "approval_required": False,
            }])

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        calls = client.list_tool_calls("run-1")
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0].tool_name, "run_command")
        self.assertTrue(calls[0].mutating)

    def test_append_artifact(self):
        def handler(request):
            self.assertEqual(request.url.path, "/v1/runs/run-1/artifacts")
            self.assertEqual(request.url.params["app_id"], "app-a")
            body = json.loads(request.content)
            self.assertEqual(body["artifact_type"], "usermaven_visual_report")
            self.assertEqual(body["format"], "json")
            self.assertEqual(body["storage_mode"], "inline")
            self.assertEqual(body["inline_content"], "{\"report_type\":\"trend\"}")
            self.assertEqual(body["metadata"], {"source": "usermaven"})
            return httpx.Response(201, json={
                "id": "art-1",
                "app_id": "app-a",
                "run_id": "run-1",
                "artifact_type": "usermaven_visual_report",
                "format": "json",
                "storage_mode": "inline",
                "inline_content": "{\"report_type\":\"trend\"}",
                "metadata": {"source": "usermaven"},
                "sequence_no": 1,
            })

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        artifact = client.append_artifact(
            "run-1",
            "usermaven_visual_report",
            "{\"report_type\":\"trend\"}",
            metadata={"source": "usermaven"},
        )
        self.assertEqual(artifact.id, "art-1")
        self.assertEqual(artifact.artifact_type, "usermaven_visual_report")

    def test_list_run_tools(self):
        def handler(request):
            self.assertEqual(request.url.path, "/v1/runs/run-1/tools")
            self.assertEqual(request.url.params["app_id"], "app-a")
            return httpx.Response(200, json={"tools": [{
                "name": "workspace.read_file",
                "description": "Read a workspace file.",
                "category": "Workspace",
                "input_schema": {"type": "object"},
                "mutating": False,
                "supported_target_types": ["repository"],
            }]})

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        tools = client.list_run_tools("run-1")
        self.assertEqual(len(tools), 1)
        self.assertEqual(tools[0].name, "workspace.read_file")
        self.assertEqual(tools[0].supported_target_types, ["repository"])

    def test_call_run_tool(self):
        def handler(request):
            self.assertEqual(request.url.path, "/v1/runs/run-1/tools")
            body = json.loads(request.content)
            self.assertEqual(body["tool_name"], "workspace.read_file")
            self.assertEqual(body["input"], {"path": "README.md"})
            return httpx.Response(200, json={
                "content": [{"type": "text", "text": "{\"content\":\"hello\"}"}],
                "is_error": False,
                "approval_required": False,
            })

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        result = client.call_run_tool("run-1", "workspace.read_file", {"path": "README.md"})
        self.assertFalse(result.is_error)
        self.assertEqual(result.content[0].text, "{\"content\":\"hello\"}")

    def test_codex_device_code_auth(self):
        seen = []

        def handler(request):
            seen.append(request.url.path)
            if request.url.path.endswith("/start"):
                return httpx.Response(200, json={
                    "provider": "openai",
                    "auth_mode": "chatgpt_device_code",
                    "state": "pending",
                    "login_id": "login-1",
                    "verification_url": "https://example.test/device",
                    "user_code": "ABCD",
                    "updated_at": "2026-06-23T00:00:00Z",
                })
            return httpx.Response(200, json={
                "provider": "openai",
                "auth_mode": "chatgpt_device_code",
                "state": "cancelled",
                "updated_at": "2026-06-23T00:00:01Z",
            })

        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        pending = client.start_codex_device_code_auth("run-1")
        cancelled = client.cancel_codex_device_code_auth("run-1")
        self.assertEqual(pending.user_code, "ABCD")
        self.assertEqual(cancelled.state, "cancelled")
        self.assertEqual(seen, [
            "/v1/runs/run-1/codex-auth/device-code/start",
            "/v1/runs/run-1/codex-auth/device-code/cancel",
        ])

    def test_errors_surface_message(self):
        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(lambda request: httpx.Response(404, json={"error": "missing"}))),
        )
        with self.assertRaisesRegex(AgentRuntimeError, "missing"):
            client.get_run("missing")

    def test_transport_errors_surface_as_agent_runtime_errors(self):
        def handler(request):
            raise httpx.ConnectError("[Errno 61] Connection refused", request=request)

        client = AgentRuntimeClient(
            "http://localhost:8090",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )
        with self.assertRaisesRegex(AgentRuntimeError, "Connection refused"):
            client.get_run("run-1")

    def test_models_round_trip_target_context(self):
        req = TargetContextRequest(app_id="app-a", target=TargetRef(type="ticket", id="T-1"))
        self.assertEqual(req.target.type, "ticket")

    def test_host_interface_models(self):
        command = CommandExecutionRequest(
            meta={
                "app_id": "host_app",
                "run_id": "run-1",
                "target": {"type": "task", "id": "T-1"},
                "target_type": "task",
                "target_id": "T-1",
            },
            command_name="pm.update_task_state",
            input={"state_id": "done"},
        )
        self.assertEqual(command.meta.target.id, "T-1")

        workspace = PrepareWorkspaceRequest(
            app_id="app-a",
            run_id="run-1",
            agent_id="agent-1",
            runtime_kind="codex",
            target={"type": "repository", "id": "repo-1"},
            workspace_mode="repository",
            execution_config={"workspace": {"mode": "repository"}},
        )
        self.assertEqual(workspace.target.type, "repository")

        spec = RepositoryWorkspaceSpec(clone_url="https://example.test/repo.git", base_branch="main")
        self.assertEqual(spec.base_branch, "main")

        tool_call = ToolCallRequest(tool_name="search", input={"q": "term"})
        result = ToolResult(content=[{"type": "text", "text": "ok"}])
        self.assertEqual(tool_call.input["q"], "term")
        self.assertEqual(result.content[0].text, "ok")

        lookup = SkillLookupRequest(app_id="app-a", key="review_agent")
        skill = WorkspaceSkill(
            id="skill-1",
            key="review_agent",
            version_key="v1",
            title="Review",
            description="Review code",
            source_kind="workspace",
            instructions="Review carefully.",
        )
        self.assertEqual(lookup.key, skill.key)

    def test_app_config_model_matches_env_shape(self):
        cfg = AppConfig(apps=[{
            "app_id": "host_app",
            "context_endpoint": "https://host.internal/agent-runtime/target-context",
            "mcp_providers": [{
                "name": "content",
                "transport": "streamable_http",
                "url": "https://host.internal/mcp",
                "tool_prefix": "content",
                "allowed_tools": ["search_articles"],
            }],
            "command_provider": {
                "transport": "http",
                "base_url": "https://host.internal/agent-runtime/commands",
            },
            "workspace_provider": {
                "transport": "repository",
                "base_url": "https://host.internal/agent-runtime/workspaces",
                "root_dir": "/tmp/agent-runtime-workspaces",
            },
            "skill_provider": {
                "transport": "http",
                "base_url": "https://host.internal/agent-runtime/skills",
                "package_base_url": "https://host.internal/agent-runtime/skill-packages",
            },
        }])
        self.assertEqual(cfg.apps[0].mcp_providers[0].allowed_tools, ["search_articles"])
        try:
            payload = cfg.model_dump(exclude_none=True)
        except AttributeError:
            payload = cfg.dict(exclude_none=True)
        self.assertEqual(payload["apps"][0]["workspace_provider"]["transport"], "repository")

        default_cfg = MCPProviderConfig(name="content", url="https://host.internal/agent-runtime/mcp/content")
        self.assertEqual(default_cfg.transport, "http")

    def test_fastapi_helpers_require_optional_dependency(self):
        with self.assertRaisesRegex(RuntimeError, r"agent-runtime\[fastapi\]"):
            create_fastapi_mcp_provider_router(lambda: [], lambda request: ToolResult())

    def test_verify_bearer_token(self):
        verify_bearer_token("Bearer secret", "secret")
        with self.assertRaises(PermissionError):
            verify_bearer_token("Bearer wrong", "secret")

    def test_tool_call_request_accepts_meta(self):
        request = ToolCallRequest(**{
            "tool_name": "analytics.query_trends",
            "input": {"query": "sessions"},
            "meta": {
                "app_id": "usermaven",
                "run_id": "run-1",
                "agent_id": "agent-1",
                "external_actor_id": "user-1",
                "workspace_id": "ws-1",
                "target": {"type": "workspace", "id": "ws-1"},
                "run_input_metadata": {"workspace_id": "ws-1"},
                "target_metadata": {},
            },
        })
        self.assertEqual(request.meta.app_id, "usermaven")
        self.assertEqual(request.meta.workspace_id, "ws-1")
        self.assertEqual(request.meta.target.type, "workspace")


if __name__ == "__main__":
    unittest.main()
