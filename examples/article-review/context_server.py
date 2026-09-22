"""Local-only context fixture for docs/quickstart.md; not a production server."""

import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

ARTICLE = {
    "title": "Export your contacts",
    "body": (
        "Open Contacts and choose Export. The export uses the filters currently "
        "selected. Files contain up to 10,000 contacts. For a larger list, split "
        "it into smaller filtered exports."
    ),
}


class ContextHandler(BaseHTTPRequestHandler):
    def respond(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path != "/context":
            return self.respond(404, {"error": "unknown endpoint"})
        expected = "Bearer " + os.environ["EXAMPLE_CONTEXT_TOKEN"]
        if not hmac.compare_digest(self.headers.get("Authorization", ""), expected):
            return self.respond(401, {"error": "unauthorized"})
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 65536:
                return self.respond(400, {"error": "invalid request size"})
            request = json.loads(self.rfile.read(length))
        except (ValueError, UnicodeDecodeError):
            return self.respond(400, {"error": "invalid JSON"})
        target = {"type": "article", "id": "export-guide"}
        if not isinstance(request, dict) or request.get("app_id") != "readme_demo" or request.get("target") != target:
            return self.respond(404, {"error": "unknown app or article"})
        self.respond(200, {
            "target": target,
            # Native execution and get_context read summary; data alone is not a prompt.
            "summary": f"Draft article to review:\nTitle: {ARTICLE['title']}\n\n{ARTICLE['body']}",
            "data": {"article": ARTICLE},
        })


if __name__ == "__main__":
    if not os.environ.get("EXAMPLE_CONTEXT_TOKEN"):
        raise SystemExit("Set EXAMPLE_CONTEXT_TOKEN before starting the example host.")
    port = int(os.environ.get("EXAMPLE_CONTEXT_PORT", "8092"))
    print(f"Sample article context: http://127.0.0.1:{port}/context", flush=True)
    HTTPServer(("127.0.0.1", port), ContextHandler).serve_forever()
