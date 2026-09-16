#!/usr/bin/env python3
"""Exercise a built CLI against an independent loopback OAuth/model fixture.

Usage: python3 scripts/cli-smoke.py /path/to/agent-runtime-cli
Uses a scripted model; does not require provider credentials.
"""
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import urllib.parse
import urllib.request


def main():
    binary = str(Path(sys.argv[1]).resolve())
    runs = {}
    grants = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def send(self, value, status=200):
            data = json.dumps(value).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path == "/agent-runtime/cli.json":
                return self.send(dict(protocol_version="agent-runtime-cli/v1alpha1",
                    app_id="smoke-host", name="Independent Smoke Host", api_base_url=base+"/cli",
                    issuer=base, client_id="public-cli", resource=base+"/cli", scopes=["runs"]))
            if parsed.path == "/.well-known/oauth-authorization-server":
                return self.send(dict(issuer=base, authorization_endpoint=base+"/authorize",
                    token_endpoint=base+"/token", code_challenge_methods_supported=["S256"]))
            if parsed.path == "/authorize":
                q = urllib.parse.parse_qs(parsed.query)
                code = "fixture-code"
                grants[code] = q
                location = q["redirect_uri"][0] + "?" + urllib.parse.urlencode(dict(
                    state=q["state"][0], code=code, iss=base))
                self.send_response(302)
                self.send_header("Location", location)
                return self.end_headers()
            if self.headers.get("Authorization") != "Bearer fixture-user-token":
                return self.send({}, 401)
            if parsed.path == "/cli/agents":
                return self.send(dict(agents=[dict(id="coder", name="Fixture Coder")]))
            if parsed.path == "/cli/me":
                return self.send(dict(id="fixture-user"))
            self.send({}, 404)

        def do_POST(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            if self.path == "/token":
                form = urllib.parse.parse_qs(body.decode())
                grant = grants.get(form.get("code", [""])[0])
                challenge = base64.urlsafe_b64encode(hashlib.sha256(
                    form.get("code_verifier", [""])[0].encode()).digest()).decode().rstrip("=")
                if not grant or challenge != grant["code_challenge"][0] or form["redirect_uri"] != grant["redirect_uri"]:
                    return self.send({}, 400)
                return self.send(dict(access_token="fixture-user-token", token_type="Bearer", expires_in=3600))
            if self.headers.get("Authorization") != "Bearer fixture-user-token":
                return self.send({}, 401)
            data = json.loads(body)
            if self.path == "/cli/runs":
                run_id = "host-" + str(len(runs) + 1)
                runs[run_id] = dict(step=0, agent=data["agent_id"], synced=False)
                return self.send(dict(run_id=run_id, agent=dict(id=data["agent_id"],
                    name="Fixture Coder", approval_mode="never", system_prompt="Follow the user task."),
                    allowed_tools=["read_files", "write_file", "run_command", "edit_file"]))
            parts = self.path.split("/")
            if len(parts) != 5 or parts[3] not in runs:
                return self.send({}, 404)
            run = runs[parts[3]]
            if parts[4] == "events":
                run["synced"] = bool(data["events"])
                return self.send({})
            if parts[4] != "model":
                return self.send({}, 404)
            run["step"] += 1
            step = run["step"]
            if run["agent"] == "approval" and step == 1:
                name, inputs = "write_file", dict(path="approved.txt", content="approved\n")
            elif run["agent"] == "approval":
                return self.send(dict(message=dict(role="assistant", content="Created approved.txt.")))
            elif step == 1:
                name, inputs = "read_files", dict(files=[dict(path="calc.py")])
            elif step == 2:
                name, inputs = "write_file", dict(path="calc.py", content="def add(a, b):\n    return a + b\n")
            elif step == 3:
                name, inputs = "run_command", dict(program="python3", args=["-c",
                    "from calc import add; assert add(2,3)==5; print('tests passed')"])
            else:
                return self.send(dict(message=dict(role="assistant", content="Finished the fixture task.")))
            self.send(dict(message=dict(role="assistant", blocks=[dict(type="tool_call",
                tool_call_id="call-"+str(step), tool_name=name, input=inputs)])))

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    base = "http://127.0.0.1:" + str(server.server_port)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="agent-runtime-cli-smoke-") as tmp:
            root = Path(tmp)
            repo = root / "repo"
            repo.mkdir()
            env = dict(os.environ, AGENT_RUNTIME_CLI_HOME=str(root / "state"))
            # Browser launch is unnecessary in this headless fixture.
            env["BROWSER"] = "true"

            def cli(*args):
                result = subprocess.run([binary, *args], env=env, cwd=repo,
                    capture_output=True, text=True, timeout=30)
                if result.returncode:
                    raise AssertionError(f"CLI failed: {args}\n{result.stdout}\n{result.stderr}")
                return result.stdout

            cli("connect", base, "--name", "fixture", "--credential-store", "file")
            process = subprocess.Popen([binary, "login", "fixture"], env=env, cwd=repo,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            assert process.stdout.readline().startswith("Open this URL")
            auth_url = process.stdout.readline().strip()
            with urllib.request.urlopen(auth_url, timeout=10) as response:
                assert response.status == 200
            stdout, stderr = process.communicate(timeout=15)
            assert process.returncode == 0, stderr
            assert "Signed in" in stdout
            assert "coder" in cli("agents", "fixture")
            assert "fixture-user" in cli("whoami", "fixture")

            def run(agent="coder", review=False, yes=True):
                args = ["review" if review else "run", "--connection", "fixture", "--agent", agent,
                        "--target", "opaque:fixture", "--json"]
                if yes:
                    args.append("--yes")
                lines = cli(*args, "Fix and test addition").splitlines()
                events = [json.loads(line) for line in lines]
                assert events[-1]["type"] == "cli.result"
                return events[-1]["run"], events

            original = "def add(a,b): return a-b\n"
            (repo / "calc.py").write_text(original)
            result, events = run()
            assert result["status"] == "completed"
            assert "a + b" in (repo / "calc.py").read_text()
            assert any(e["type"] == "command.output" and "tests passed" in e["data"]["text"] for e in events)
            assert result["id"] in cli("runs", "list")
            assert json.loads(cli("runs", "show", result["id"]))["run"]["status"] == "completed"
            cli("runs", "sync", result["id"])
            assert all(r["synced"] for r in runs.values())

            (repo / "calc.py").write_text(original)
            result, _ = run(review=True)
            assert (repo / "calc.py").read_text() == original, "review edited a file"

            result, _ = run(agent="approval", yes=False)
            assert result["status"] == "paused"
            assert not (repo / "approved.txt").exists()
            cli("runs", "resume", result["id"], "--intent", "approve", "--json")
            assert (repo / "approved.txt").read_text() == "approved\n"
            cli("logout", "fixture")
            print("PASS: built CLI OAuth, coding, real tests, JSON, saved runs, sync, review, approval/restart, logout")
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
