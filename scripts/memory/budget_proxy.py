"""Loopback-only OpenRouter relay with conservative reservations and cost audit.

No prompts, responses, or credentials enter the audit. Request payloads and model
settings are forwarded unchanged. A missing cost consumes its reservation.
"""
import hmac
import json
import math
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import threading
import urllib.error
import urllib.request


class Ledger:
    def __init__(self, limit, audit):
        if not math.isfinite(limit) or limit <= 0:
            raise ValueError("cost limit must be positive and finite")
        self.limit, self.audit = limit, Path(audit)
        self.spent, self.reserved, self.rows = 0.0, 0.0, []
        self.lock = threading.Lock()

    def reserve(self, amount):
        if not math.isfinite(amount) or amount < 0:
            raise ValueError("invalid cost reservation")
        with self.lock:
            if self.spent + self.reserved + amount > self.limit:
                return False
            self.reserved += amount
            return True

    def complete(self, reserved, model, status, usage):
        cost = usage.get("cost") if isinstance(usage, dict) else None
        measured = isinstance(cost, (int, float)) and math.isfinite(cost) and cost >= 0
        with self.lock:
            self.reserved = max(0, self.reserved - reserved)
            self.spent += cost if measured else reserved
            self.rows.append({"model": model, "status": status, "cost_usd": cost if measured else None,
                              "charged_to_budget_usd": cost if measured else reserved,
                              "prompt_tokens": usage.get("prompt_tokens") if usage else None,
                              "completion_tokens": usage.get("completion_tokens") if usage else None})
            self.audit.write_text(json.dumps({"limit_usd": self.limit, "charged_usd": self.spent,
                                             "reserved_usd": self.reserved, "requests": self.rows}, indent=2)+"\n")


class BudgetProxy:
    def __init__(self, key, limit, models, audit):
        self.key, self.models = key, models
        self.ledger = Ledger(limit, audit)
        proxy = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def respond(self, code, data):
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self):
                if self.path not in ("/v1/chat/completions", "/v1/embeddings"):
                    self.respond(404, b'{"error":{"message":"unsupported benchmark route"}}')
                    return
                if not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer "+proxy.key):
                    self.respond(401, b'{"error":{"message":"benchmark relay authentication required"}}')
                    return
                size = int(self.headers.get("Content-Length", "0"))
                if size < 1 or size > 16 << 20:
                    self.respond(400, b'{"error":{"message":"invalid benchmark request size"}}')
                    return
                data = self.rfile.read(size)
                body = json.loads(data)
                model = body.get("model")
                if model not in proxy.models or body.get("stream"):
                    self.respond(400, b'{"error":{"message":"unpriced model or streaming request rejected"}}')
                    return
                pricing = proxy.models[model]
                output = body.get("max_completion_tokens", body.get("max_tokens", 0))
                if self.path.endswith("completions") and (not isinstance(output, int) or output < 1):
                    self.respond(400, b'{"error":{"message":"explicit output token cap required"}}')
                    return
                # UTF-8 bytes upper-bound input tokens, including JSON overhead.
                # Use the highest catalog tier for the requested model.
                reserved = size * pricing["prompt"] + output * pricing["completion"]
                if not proxy.ledger.reserve(reserved):
                    self.respond(402, b'{"error":{"message":"benchmark cost cap reached"}}')
                    return
                status, usage = 502, None
                try:
                    request = urllib.request.Request("https://openrouter.ai/api"+self.path, data=data,
                              headers={"Authorization":"Bearer "+proxy.key, "Content-Type":"application/json"})
                    try:
                        with urllib.request.urlopen(request, timeout=600) as response:
                            status, result = response.status, response.read()
                    except urllib.error.HTTPError as error:
                        status, result = error.code, error.read()
                    decoded = json.loads(result)
                    usage = decoded.get("usage")
                    proxy.ledger.complete(reserved, model, status, usage)
                    self.respond(status, result)
                except (urllib.error.URLError, TimeoutError, ValueError):
                    proxy.ledger.complete(reserved, model, status, usage)
                    self.respond(502, b'{"error":{"message":"benchmark upstream request failed"}}')

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self):
        self.thread.start()
        return f"http://127.0.0.1:{self.server.server_port}/v1"

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


def catalog_prices(model_ids):
    with urllib.request.urlopen("https://openrouter.ai/api/v1/models", timeout=30) as response:
        catalog = {model["id"]: model for model in json.load(response)["data"]}
    with urllib.request.urlopen("https://openrouter.ai/api/v1/embeddings/models", timeout=30) as response:
        catalog.update({model["id"]: model for model in json.load(response)["data"]})
    prices = {}
    for model in model_ids:
        pricing = catalog[model]["pricing"]
        tiers = [pricing] + pricing.get("overrides", [])
        prices[model] = {kind:max(float(tier.get(kind, pricing[kind])) for tier in tiers)
                         for kind in ("prompt", "completion")}
    return prices
