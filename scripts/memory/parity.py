#!/usr/bin/env python3
"""Paired live LongMemEval evaluation; never promotes a pilot to parity.

Run with the pinned Hindsight checkout's Python (rich and tiktoken required).
Uses the pinned AMB dataset loader, answer prompt and category-specific judge
without copying or modifying benchmark logic. Each question gets fresh banks.
"""
import argparse
import atexit
from collections import Counter
from dataclasses import replace
import hashlib
import importlib
import ipaddress
from urllib.parse import urlparse
import json
import os
from pathlib import Path
import random
import socket
import subprocess
import sys
import tempfile
import time
import types
import urllib.error
import urllib.request

HINDSIGHT_SHA = "017b3f5d888d67341e70f43c102cb8155bb9f51e"
AMB_SHA = "f618ed7b1f0eb9cad7b42e876f91a42f0eadb150"
DATA_SHA = "d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442"
ROOT = Path(__file__).resolve().parents[2]
CATEGORIES = ["single-session-user", "single-session-assistant", "multi-session",
              "temporal-reasoning", "knowledge-update", "single-session-preference"]


def git(path, *args):
    return subprocess.check_output(["git", "-C", str(path), *args], text=True).strip()


def verify_source(path, sha):
    if git(path, "rev-parse", "HEAD") != sha:
        raise ValueError(f"{path}: expected pinned commit {sha}")
    if git(path, "status", "--porcelain", "--untracked-files=no"):
        raise ValueError(f"{path}: tracked source modifications invalidate reference")


def http(url, body=None, key=""):
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    request = urllib.request.Request(url, data=json.dumps(body).encode() if body is not None else None,
                                     headers=headers)
    with urllib.request.urlopen(request, timeout=600) as response:
        return json.load(response)


def chat(url, model, prompt, key, options, schema=None):
    body = dict(options)
    body.update(model=model, messages=[{"role": "user", "content": prompt}])
    if schema:
        body["response_format"] = {"type": "json_schema", "json_schema":
                                   {"name": "judgment", "strict": True, "schema": schema}}
    response = http(url.rstrip("/") + "/chat/completions", body, key)
    choice = response["choices"][0]
    if choice.get("finish_reason") != "stop" or not choice["message"].get("content"):
        raise ValueError("answer/judge generation incomplete")
    return choice["message"]["content"]


def confidence(differences):
    if not differences:
        return None
    rng = random.Random(20261002)
    means = sorted(sum(rng.choices(differences, k=len(differences))) / len(differences)
                   for _ in range(10000))
    return {"mean": sum(differences)/len(differences),
            "paired_bootstrap_95_percent": [means[250], means[9749]], "seed": 20261002}


def unique_occurrences(documents, gold_ids):
    """Preserve repeated source occurrences, including distinct timestamps."""
    counts = Counter(doc.id for doc in documents)
    seen, aliases, output, expected = Counter(), {}, [], []
    for doc in documents:
        seen[doc.id] += 1
        normalized = doc.id if counts[doc.id] == 1 else f"{doc.id}__occurrence_{seen[doc.id]}"
        output.append(replace(doc, id=normalized))
        if normalized != doc.id:
            aliases[normalized] = doc.id
        if doc.id in gold_ids:
            expected.append(normalized)
    return output, expected, aliases


def benchmark_answer_context(adapter, evidence):
    """Use the pinned adapter's wrapping, chunk deduplication and formatting."""
    response = adapter._as_recall_response(evidence)
    documents = adapter._build_docs(adapter._deduplicate_results(response.results), response.chunks)
    return "\n\n".join(f"## Memory {i + 1}\n{document.content}" for i, document in enumerate(documents))


def require_loopback(url):
    parsed = urlparse(url)
    try:
        local = parsed.hostname == "localhost" or ipaddress.ip_address(parsed.hostname).is_loopback
    except ValueError:
        local = False
    if parsed.scheme not in ("http", "https") or not local or parsed.username or parsed.password:
        raise ValueError("local-only inference requires a loopback HTTP(S) endpoint")


def resumed_rows(previous, current, selected):
    for field, value in current.items():
        if field not in {"questions", "reference_deployment_verified"} and previous.get(field) != value:
            raise ValueError(f"resume profile mismatch: {field}")
    rows = [row for row in previous.get("questions", []) if "error" not in row and
            set(row.get("backends", {})) == {"local", "hindsight"} and
            all(type(row["backends"][name].get("judgment", {}).get("correct")) is bool
                for name in ("local", "hindsight"))]
    if any(row["question_id"] not in selected for row in rows):
        raise ValueError("resume question selection changed")
    if len({row["question_id"] for row in rows}) != len(rows):
        raise ValueError("resume has duplicate completed questions")
    return rows


def gate(report):
    reasons = []
    rows = report["questions"]
    if not report.get("reference_deployment_verified"):
        reasons.append("the live reference deployment must be verified from pinned source")
    if report["history_mode"] != "full":
        reasons.append("oracle histories omit distractors and cannot establish benchmark parity")
    completed = [row for row in rows if "error" not in row and
                 set(row.get("backends", {})) == {"local", "hindsight"} and
                 all(type(row["backends"][name].get("judgment", {}).get("correct")) is bool
                     for name in ("local", "hindsight"))]
    if len(completed) != 500 or len({row.get("question_id") for row in completed}) != 500:
        reasons.append("all 500 questions must complete successfully")
    if set(row["category"] for row in rows) != set(CATEGORIES):
        reasons.append("all six question categories are required")
    if not report["independent_judge"]:
        reasons.append("answer and judge use the same model; independent adjudication is required")
    ci = report.get("answer_accuracy_difference")
    if not ci or ci["paired_bootstrap_95_percent"][0] < -0.05:
        reasons.append("answer accuracy noninferiority lower bound must be at least -5 percentage points")
    report["retain_recall_quality_gate"] = {"passed": not reasons, "reasons": reasons,
                                          "noninferiority_margin": 0.05}
    report["full_hindsight_parity"] = {"established": False, "missing_capabilities":
        ["full upstream observation consolidation", "full upstream Reflect", "full upstream mental models", "multimodal retention",
         "upstream keyword scoring and entity resolution", "complete upstream temporal parsing"]}


def write_report(path, report):
    gate(report)
    target = path.with_suffix(path.suffix + ".tmp")
    target.write_text(json.dumps(report, indent=2) + "\n")
    target.replace(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--hindsight-source", type=Path, required=True)
    parser.add_argument("--benchmark-source", type=Path, required=True)
    parser.add_argument("--dataset", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--history", choices=["full", "oracle"], default="full")
    parser.add_argument("--per-category", type=int, help="bounded diagnostic pilot; default all 500")
    parser.add_argument("--question-id", action="append", help="evaluate only named IDs as a targeted diagnostic")
    parser.add_argument("--answer-context", choices=["amb", "json"], default="amb",
                        help="AMB markdown context (default); JSON reproduces historical diagnostics")
    parser.add_argument("--include-chunks", action="store_true", help="include retrieved source chunks alongside facts")
    parser.add_argument("--retain-workers", type=int, default=1, help="parallel source documents per backend (1–16)")
    parser.add_argument("--reference-port", type=int, default=18889)
    parser.add_argument("--answer-model", required=True)
    parser.add_argument("--judge-model", required=True)
    parser.add_argument("--env-file", type=Path, help="local credentials/config; loaded without executing shell code")
    parser.add_argument("--local-only", action="store_true", help="reject non-loopback inference endpoints")
    parser.add_argument("--work-dir", type=Path, help="persist per-question SQLite files so partial ingestion can resume")
    parser.add_argument("--evaluator-binary", type=Path, help="use a frozen evaluator executable for a long-running comparison")
    parser.add_argument("--resume", action="store_true", help="resume completed questions only when the full evaluation profile and binary match")
    parser.add_argument("--reuse-retained-from", type=Path, help="reuse bank IDs and retained state from a compatible local report; recompute every answer/judgment")
    parser.add_argument("--max-cost", type=float, help="OpenRouter cost cap in USD; requires explicit output limits")
    args = parser.parse_args()
    if not 1 <= args.retain_workers <= 16:
        raise ValueError("retain-workers must be 1–16")
    verify_source(args.hindsight_source, HINDSIGHT_SHA)
    verify_source(args.benchmark_source, AMB_SHA)
    if hashlib.sha256(args.dataset.read_bytes()).hexdigest() != DATA_SHA:
        raise ValueError("LongMemEval dataset differs from pinned corpus")
    if args.per_category is not None and args.per_category < 1:
        raise ValueError("per-category must be positive")
    # Bypass AMB's registry imports (which load every other dataset and scipy).
    # The selected dataset module and its dependencies remain unmodified.
    sys.path.insert(0, str(args.benchmark_source / "src"))
    package = types.ModuleType("memory_bench.dataset")
    package.__path__ = [str(args.benchmark_source / "src/memory_bench/dataset")]
    sys.modules["memory_bench.dataset"] = package
    os.environ["LONGMEMEVAL_DATA_PATH"] = str(args.dataset)
    dataset = importlib.import_module("memory_bench.dataset.longmemeval").LongMemEvalDataset()
    # Load only the pinned Hindsight adapter, bypassing optional provider registries.
    memory_package = types.ModuleType("memory_bench.memory")
    memory_package.__path__ = [str(args.benchmark_source / "src/memory_bench/memory")]
    sys.modules["memory_bench.memory"] = memory_package
    adapter = importlib.import_module("memory_bench.memory.hindsight")
    if not Path(adapter.__file__).resolve().is_relative_to(args.benchmark_source.resolve()):
        raise ValueError("answer formatter does not import pinned benchmark source")
    formatting_fixture = json.loads(Path(__file__).with_name("answer-context-fixture.json").read_text())
    if (formatting_fixture["benchmark_commit"] != AMB_SHA or
            benchmark_answer_context(adapter, formatting_fixture["evidence"]) != formatting_fixture["expected_context"]):
        raise ValueError("pinned benchmark answer-context fixture failed")
    queries = dataset.load_queries("s")
    if args.per_category:
        # Deterministic pilot selection by ID, not answer or successful outcome.
        queries = [q for category in CATEGORIES for q in
                   sorted((q for q in queries if q.meta["question_type"] == category), key=lambda q:q.id)
                   [:args.per_category]]
    if args.question_id:
        selected_ids = set(args.question_id)
        queries = [q for q in queries if q.id in selected_ids]
        if {q.id for q in queries} != selected_ids:
            raise ValueError("unknown or excluded question ID")
    documents = dataset.load_documents("s", user_ids={q.id for q in queries})
    env = dict(os.environ)
    if args.env_file:
        from dotenv import dotenv_values
        env.update({key: value for key, value in dotenv_values(args.env_file, interpolate=False).items()
                    if value is not None})
    if env.get("OPENROUTER_API_KEY"):
        env.setdefault("AGENT_RUNTIME_MEMORY_MODEL_URL", "https://openrouter.ai/api/v1")
        if env["AGENT_RUNTIME_MEMORY_MODEL_URL"].rstrip("/") == "https://openrouter.ai/api/v1":
            env.setdefault("AGENT_RUNTIME_MEMORY_MODEL_API_KEY", env["OPENROUTER_API_KEY"])
    env.setdefault("AGENT_RUNTIME_MEMORY_MODEL_TIMEOUT", "15m")
    env.setdefault("AGENT_RUNTIME_MEMORY_CANDIDATE_LIMIT", "1000")
    model_url = env["AGENT_RUNTIME_MEMORY_MODEL_URL"]
    key = env.get("AGENT_RUNTIME_MEMORY_MODEL_API_KEY", "")
    embedding_url = env.get("AGENT_RUNTIME_MEMORY_EMBEDDING_URL") or model_url
    embedding_key = env.get("AGENT_RUNTIME_MEMORY_EMBEDDING_API_KEY", "") if env.get("AGENT_RUNTIME_MEMORY_EMBEDDING_URL") else key
    if args.local_only:
        require_loopback(model_url)
        require_loopback(embedding_url)
        if args.max_cost is not None:
            raise ValueError("local-only profile cannot use a hosted cost proxy")
        key = embedding_key = "local"
        env["AGENT_RUNTIME_MEMORY_MODEL_API_KEY"] = key
        env["AGENT_RUNTIME_MEMORY_EMBEDDING_API_KEY"] = embedding_key
        env.pop("OPENROUTER_API_KEY", None)
    if args.resume and not args.local_only:
        raise ValueError("resume is currently restricted to local-only runs; hosted cumulative budget accounting is required")
    options = json.loads(env.get("AGENT_RUNTIME_MEMORY_EXTRACTION_OPTIONS", "{}"))
    answer_options = json.loads(env.get("AGENT_RUNTIME_MEMORY_ANSWER_OPTIONS", json.dumps(options)))
    judge_options = json.loads(env.get("AGENT_RUNTIME_MEMORY_JUDGE_OPTIONS", json.dumps(options)))
    gateway = None
    if args.max_cost is not None:
        if model_url.rstrip("/") != "https://openrouter.ai/api/v1" or embedding_url.rstrip("/") != model_url.rstrip("/"):
            raise ValueError("cost-capped profile requires both inference endpoints to use OpenRouter")
        from budget_proxy import BudgetProxy, catalog_prices
        models = {env["AGENT_RUNTIME_MEMORY_EXTRACTION_MODEL"], env["AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL"],
                  args.answer_model, args.judge_model}
        gateway = BudgetProxy(key, args.max_cost, catalog_prices(models), args.output.with_suffix(".cost.json"))
        atexit.register(gateway.close)
        model_url = embedding_url = gateway.start()
        env["AGENT_RUNTIME_MEMORY_MODEL_URL"] = model_url
        env["AGENT_RUNTIME_MEMORY_EMBEDDING_URL"] = embedding_url
        env["AGENT_RUNTIME_MEMORY_EMBEDDING_API_KEY"] = embedding_key
    port = args.reference_port
    with socket.socket() as reservation:
        reservation.bind(("127.0.0.1", port))
    reference_url = f"http://127.0.0.1:{port}"
    env.update(HINDSIGHT_API_HOST="127.0.0.1", HINDSIGHT_API_PORT=str(port),
               HINDSIGHT_API_LLM_PROVIDER="openai", HINDSIGHT_API_LLM_BASE_URL=model_url,
               HINDSIGHT_API_LLM_MODEL=env["AGENT_RUNTIME_MEMORY_EXTRACTION_MODEL"],
               HINDSIGHT_API_LLM_API_KEY=key or "local",
               HINDSIGHT_API_LLM_EXTRA_BODY=json.dumps(options), HINDSIGHT_API_LLM_TEMPERATURE="none",
               HINDSIGHT_API_RETAIN_MAX_COMPLETION_TOKENS=str(options.get("max_completion_tokens", options.get("max_tokens", 16384))),
               HINDSIGHT_API_LLM_STRICT_SCHEMA="true", HINDSIGHT_API_EMBEDDINGS_PROVIDER="openai",
               HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=embedding_url,
               HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL=env["AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL"],
               HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY=embedding_key or "local",
               HINDSIGHT_API_RERANKER_PROVIDER="rrf", HINDSIGHT_API_ENABLE_OBSERVATIONS="false",
               AGENT_RUNTIME_MEMORY_HINDSIGHT_URL=reference_url)
    if env.get("AGENT_RUNTIME_MEMORY_RERANK_MODEL"):
        raise ValueError("this RRF profile requires no local rerank model")
    if env.get("AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS"):
        env["HINDSIGHT_API_EMBEDDINGS_OPENAI_DIMENSIONS"] = env["AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS"]
    report = {"dataset": "longmemeval_s_cleaned", "dataset_sha256": DATA_SHA,
              "hindsight_commit": HINDSIGHT_SHA, "benchmark_commit": AMB_SHA,
              "model_url": model_url, "embedding_url": embedding_url,
              "local_only": args.local_only,
              "history_mode": args.history, "reference_deployment_verified": False,
              "extraction_model": env["AGENT_RUNTIME_MEMORY_EXTRACTION_MODEL"],
              "embedding_model": env["AGENT_RUNTIME_MEMORY_EMBEDDING_MODEL"],
              "embedding_dimensions": env.get("AGENT_RUNTIME_MEMORY_EMBEDDING_DIMENSIONS", "provider default"),
              "extraction_options": options, "reranker": "rrf (no neural reranking)",
              "answer_context_format": args.answer_context,
              "answer_options": answer_options, "judge_options": judge_options,
              "retain_workers": args.retain_workers, "observations_enabled": False, "answer_model": args.answer_model,
              "judge_model": args.judge_model, "independent_judge": args.answer_model != args.judge_model,
              "local_candidate_limit": env["AGENT_RUNTIME_MEMORY_CANDIDATE_LIMIT"],
              "recall_include_chunks": args.include_chunks, "recall_max_chunk_tokens": 16384 if args.include_chunks else 0,
              "recall_max_tokens": 32768, "recall_limit": 100, "questions": []}
    report["budget_profile"] = "o200k_base token count of serialized JSON in both Go adapters"
    report["memory_request_timeout"] = env["AGENT_RUNTIME_MEMORY_MODEL_TIMEOUT"]
    report["cost_limit_usd"] = args.max_cost
    args.output.parent.mkdir(parents=True, exist_ok=True)
    judge_schema = {"type":"object", "additionalProperties":False,
                    "required":["reasoning", "correct"], "properties":
                    {"reasoning":{"type":"string"}, "correct":{"type":"boolean"}}}
    retained = {}
    if args.reuse_retained_from:
        if not args.local_only or not args.work_dir:
            raise ValueError("retained-state reuse requires local-only and persistent state")
        retained = json.loads(args.reuse_retained_from.read_text())
        for field in ("dataset_sha256", "hindsight_commit", "benchmark_commit", "history_mode", "extraction_model", "embedding_model", "embedding_dimensions", "extraction_options", "observations_enabled", "local_only"):
            if retained.get(field) != report.get(field):
                raise ValueError("retained ingestion profile differs: " + field)
        if not retained.get("reference_deployment_verified"):
            raise ValueError("retained reference was not verified")
        report["retained_state_provenance"] = {"report":str(args.reuse_retained_from.resolve()), "sha256":hashlib.sha256(args.reuse_retained_from.read_bytes()).hexdigest(), "evaluator_sha256":retained.get("runtime_evaluator_sha256"), "judgments_reused":False}
    with tempfile.TemporaryDirectory(prefix="memory-parity-") as tmp:
        tmp = Path(tmp)
        binary = args.evaluator_binary.resolve() if args.evaluator_binary else tmp / "memory-eval"
        if not args.evaluator_binary:
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/memory-eval"], cwd=ROOT, check=True)
        if args.work_dir:
            args.work_dir = args.work_dir.resolve()
            args.work_dir.mkdir(parents=True, exist_ok=True)
        report["work_dir"] = str(args.work_dir) if args.work_dir else None
        previous = {}
        report["runtime_evaluator_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
        if args.resume and args.output.exists():
            previous = json.loads(args.output.read_text())
            report["questions"] = resumed_rows(previous, report, {q.id for q in queries})
        python = args.hindsight_source / ".venv/bin/python"
        # Verify editable install resolves to the pinned checkout used to launch.
        imported = subprocess.check_output([str(python), "-c", "import hindsight_api; print(hindsight_api.__file__)"],
                                           env=env, text=True).strip()
        if not Path(imported).resolve().is_relative_to(args.hindsight_source.resolve()):
            raise ValueError("reference Python does not import pinned checkout")
        log_path = args.output.with_suffix(".reference.log")
        with log_path.open("w") as log:
            server = subprocess.Popen([str(python), "-m", "hindsight_api.main", "--host", "127.0.0.1", "--port", str(port)],
                                      cwd=args.hindsight_source, env=env, stdout=log, stderr=log)
            try:
                deadline = time.monotonic()+120
                while True:
                    if server.poll() is not None:
                        raise RuntimeError(f"reference exited; see {log_path}")
                    try:
                        http(reference_url + "/health")
                        if "Uvicorn running on" in log_path.read_text():
                            break
                    except (urllib.error.URLError, TimeoutError):
                        if time.monotonic() > deadline:
                            raise RuntimeError(f"reference startup timed out; see {log_path}")
                        time.sleep(1)
                report["reference_deployment_verified"] = True
                report["reference_pid"] = server.pid
                if args.include_chunks:
                    import uuid
                    probe_bank = "agent-runtime-parity-preflight-" + uuid.uuid4().hex
                    probe_url = reference_url + "/v1/default/banks/" + probe_bank
                    http(probe_url + "/memories", {"async":False,"items":[{
                        "document_id":"chunk-preflight", "content":"Alice works at Acme and leads the infrastructure team. Alice prefers Go for backend services.",
                        "timestamp":"2026-10-02T00:00:00Z"}]})
                    probe = http(probe_url + "/memories/recall", {"query":"Where does Alice work?",
                        "types":["world","experience"], "budget":"high", "max_tokens":4096,
                        "include":{"entities":None,"chunks":{"max_tokens":1024}}})
                    if not probe.get("results") or not any(chunk.get("text") for chunk in (probe.get("chunks") or {}).values()):
                        raise ValueError("reference source-chunk preflight failed; aborting uneven comparison")
                    report["reference_chunk_preflight_verified"] = True
                differences = [int(row["backends"]["local"]["judgment"]["correct"]) -
                               int(row["backends"]["hindsight"]["judgment"]["correct"])
                               for row in report["questions"]]
                done_ids = {row["question_id"] for row in report["questions"]}
                for index, query in enumerate(queries):
                    if query.id in done_ids:
                        continue
                    case_docs = [d for d in documents if d.user_id == query.id and
                                 (args.history == "full" or d.id in query.gold_ids)]
                    case_docs, expected, aliases = unique_occurrences(case_docs, set(query.gold_ids))
                    row = {"question_id": query.id, "category":query.meta["question_type"],
                           "document_count":len(case_docs), "history_bytes":sum(len(d.content.encode()) for d in case_docs),
                           "question":query.query, "gold_answers":query.gold_answers}
                    old_row = next((old for old in previous.get("questions", []) if old.get("question_id") == query.id), {})
                    if not old_row:
                        old_row = next((old for old in retained.get("questions", []) if old.get("question_id") == query.id), {})
                        if old_row:
                            state_path = args.work_dir / (old_row["evaluation_bank_id"] + ".db")
                            if not state_path.is_file():
                                raise ValueError("retained bank database is missing")
                    row["evaluation_bank_id"] = old_row.get("evaluation_bank_id") or __import__("uuid").uuid4().hex
                    row["source_occurrence_aliases"] = aliases
                    report["questions"].append(row)
                    print(f"[{index+1}/{len(queries)}] {query.id}: {len(case_docs)} documents", flush=True)
                    fixture = {"documents":[{"document_id":d.id, "content":d.content,
                                **({"timestamp":d.timestamp} if d.timestamp else {}), "context":d.context or "", "metadata":{"agent_runtime_document_id":d.id}}
                               for d in case_docs],
                               "queries":[{"name":query.id, "request":{"query":query.query, "limit":100,
                                  "max_tokens":32768, "include_chunks":args.include_chunks,
                                  **({"max_chunk_tokens":16384} if args.include_chunks else {}), **({"query_timestamp":query.meta["query_timestamp"]}
                                  if query.meta.get("query_timestamp") else {})}, "expected_documents":expected}]}
                    fixture_path = tmp / "fixture.json"
                    fixture_path.write_text(json.dumps(fixture))
                    write_report(args.output, report)
                    command = [str(binary), "-mode", "both", "-fixture", str(fixture_path),
                               "-retain-workers", str(args.retain_workers), "-bank-id", row["evaluation_bank_id"]]
                    if args.work_dir:
                        command += ["-sqlite-path", str(args.work_dir / (row["evaluation_bank_id"] + ".db"))]
                    try:
                        pair = json.loads(subprocess.check_output(command,
                                                                 env=env, cwd=ROOT, text=True))
                        row["backends"] = {}
                        for result in pair:
                            result["reference_deployment_verified"] = True
                            evidence = result["queries"][0]["evidence"]
                            context = benchmark_answer_context(adapter, evidence) if args.answer_context == "amb" else json.dumps(evidence)
                            prompt = dataset.build_rag_prompt(query.query, context, "open", "s",
                                                              query.meta["question_type"], query.meta)
                            answer = chat(model_url, args.answer_model, prompt, key, answer_options)
                            judge_prompt = dataset.get_judge_prompt_fn(query.meta["question_type"], query.meta)(
                                query.query, query.gold_answers, answer)
                            judgment = json.loads(chat(model_url, args.judge_model, judge_prompt, key, judge_options, judge_schema))
                            if type(judgment.get("correct")) is not bool:
                                raise ValueError("judge must return a boolean correctness verdict")
                            row["backends"][result["backend"]] = {"retrieval":result, "answer":answer, "judgment":judgment}
                        differences.append(int(row["backends"]["local"]["judgment"]["correct"]) -
                                           int(row["backends"]["hindsight"]["judgment"]["correct"]))
                    except (subprocess.CalledProcessError, ValueError, urllib.error.URLError, TimeoutError) as error:
                        row["error"] = f"{type(error).__name__}: evaluation failed; inspect terminal/reference log"
                    report["answer_accuracy_difference"] = confidence(differences)
                    if gateway:
                        report["charged_cost_usd"] = gateway.ledger.spent
                    complete = [r for r in report["questions"] if "error" not in r and len(r.get("backends", {})) == 2]
                    report["completed_questions"] = len(complete)
                    report["answer_accuracy"] = {name: sum(r["backends"][name]["judgment"]["correct"] for r in complete) / len(complete)
                                                 for name in ("local", "hindsight")} if complete else None
                    write_report(args.output, report)
            finally:
                server.terminate()
                try:
                    server.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    server.kill()
                    server.wait()
    print(json.dumps(report["retain_recall_quality_gate"], indent=2))
    return 0 if report["retain_recall_quality_gate"]["passed"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
