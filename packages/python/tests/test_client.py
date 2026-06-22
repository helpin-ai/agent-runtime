import json
import unittest

import httpx

from agent_runtime import Agent, AgentRuntimeClient, AgentRuntimeError, TargetContextRequest, TargetRef, verify_bearer_token


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


class ClientTests(unittest.TestCase):
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

    def test_errors_surface_message(self):
        client = AgentRuntimeClient(
            "https://runtime.internal",
            "app-a",
            client=httpx.Client(transport=httpx.MockTransport(lambda request: httpx.Response(404, json={"error": "missing"}))),
        )
        with self.assertRaisesRegex(AgentRuntimeError, "missing"):
            client.get_run("missing")

    def test_models_round_trip_target_context(self):
        req = TargetContextRequest(app_id="app-a", target=TargetRef(type="ticket", id="T-1"))
        self.assertEqual(req.target.type, "ticket")

    def test_verify_bearer_token(self):
        verify_bearer_token("Bearer secret", "secret")
        with self.assertRaises(PermissionError):
            verify_bearer_token("Bearer wrong", "secret")


if __name__ == "__main__":
    unittest.main()
