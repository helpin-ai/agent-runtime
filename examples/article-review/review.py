"""Propose edits to the sample article. Run after completing docs/quickstart.md."""

import os

import httpx
from agent_runtime import AgentRuntimeClient


def main():
    token = os.environ.get("AGENT_RUNTIME_SERVICE_TOKEN")
    if not token:
        raise SystemExit("Set AGENT_RUNTIME_SERVICE_TOKEN to match the runtime service.")

    # Bound network stalls while allowing time for the first model response.
    with httpx.Client(timeout=120) as transport, AgentRuntimeClient(
        base_url=os.environ.get("EXAMPLE_RUNTIME_URL", "http://127.0.0.1:8090"),
        app_id="readme_demo",
        service_token=token,
        client=transport,
    ) as client:
        agent = client.upsert_agent({
            "id": "article_reviewer",
            "name": "Article reviewer",
            "runtime_kind": "native_sdk",
            "system_prompt": "Review the supplied article and propose clear edits.",
            "allowed_targets": ["article"],
            "allowed_tools": ["get_context"],
        })
        run = client.start_run({
            "agent_id": agent.id,
            "target": {"type": "article", "id": "export-guide"},
            "instructions": "Review this article. Propose edits; do not publish.",
        })
        print("Run started:", run.id, flush=True)
        for event in client.iter_run_events(run.id):
            print(event.sequence_no, event.type, flush=True)
            if event.type in {"run.completed", "run.failed", "run.cancelled", "run.paused"}:
                break

        finished = client.get_run(run.id)
        print("Run status:", finished.status)
        if finished.status != "completed":
            raise SystemExit(f"Run did not complete: {finished.error_message or finished.pause_reason or finished.status}")
        for message in client.list_messages(run.id):
            if message.role == "assistant" and message.content:
                print(message.content)


if __name__ == "__main__":
    main()
