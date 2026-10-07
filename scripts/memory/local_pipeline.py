#!/usr/bin/env python3
"""Continue local complete-history pilots (6, 30), then the full 500 cases.

Preserves the frozen executable, profile and completed cases. An operational
failure stops expansion for diagnosis; incomplete pilots never count as parity.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def completed_stage(report, expected):
    if not report.get("local_only") or report.get("history_mode") != "full":
        raise ValueError("pipeline requires a local-only full-history report")
    if not report.get("reference_deployment_verified") or not report.get("independent_judge"):
        raise ValueError("verified reference and independent judge required")
    rows = report.get("questions", [])
    if len(rows) != expected or len({row.get("question_id") for row in rows}) != expected:
        raise ValueError(f"stage requires {expected} unique completed questions")
    for row in rows:
        if "error" in row or set(row.get("backends", {})) != {"local", "hindsight"}:
            raise ValueError("stage contains incomplete or failed cases; inspect report before expanding")
        if any(type(row["backends"][name].get("judgment", {}).get("correct")) is not bool
               for name in ("local", "hindsight")):
            raise ValueError("stage contains invalid judgments")


def write_status(path, **values):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(values, indent=2) + "\n")
    temporary.replace(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("hindsight-source", "benchmark-source", "dataset", "env-file",
                 "evaluator-binary", "work-dir", "output"):
        parser.add_argument("--" + flag, type=Path, required=True)
    parser.add_argument("--answer-model", required=True)
    parser.add_argument("--judge-model", required=True)
    parser.add_argument("--reference-port", type=int, default=18891)
    parser.add_argument("--retain-workers", type=int, default=2)
    parser.add_argument("--reuse-retained-from", type=Path)
    parser.add_argument("--adopt-pilot-pid", type=int, help="wait for an already running six-case pilot")
    args = parser.parse_args()
    status = args.output.with_name("pipeline.json")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    command = [sys.executable, str(Path(__file__).with_name("parity.py")),
               "--local-only", "--include-chunks", "--resume"]
    for flag in ("hindsight-source", "benchmark-source", "dataset", "env-file",
                 "evaluator-binary", "work-dir", "output", "answer-model", "judge-model",
                 "reference-port", "retain-workers"):
        command += ["--" + flag, str(getattr(args, flag.replace("-", "_")))]
    if args.reuse_retained_from:
        command += ["--reuse-retained-from", str(args.reuse_retained_from)]
    try:
        for count, per_category in ((6, 1), (30, 5), (500, None)):
            write_status(status, state="running", stage_questions=count, parity_established=False)
            if count == 6 and args.adopt_pilot_pid:
                if args.adopt_pilot_pid <= 1:
                    raise ValueError("invalid pilot PID")
                while True:
                    try:
                        os.kill(args.adopt_pilot_pid, 0)
                    except ProcessLookupError:
                        break
                    time.sleep(5)
            else:
                run = command + (["--per-category", str(per_category)] if per_category else [])
                with args.output.with_name(f"pipeline-{count}.log").open("a") as log:
                    result = subprocess.run(run, stdout=log, stderr=log)
                # Pilot gate failure (2) is expected until all 500 cases complete.
                if result.returncode not in (0, 2):
                    raise ValueError(f"evaluation process exited {result.returncode}; inspect stage log")
            report = json.loads(args.output.read_text())
            completed_stage(report, count)
            args.output.with_name(f"parity-{count}.json").write_text(json.dumps(report, indent=2) + "\n")
        passed = report["retain_recall_quality_gate"]["passed"]
        write_status(status, state="complete", stage_questions=500,
                     retain_recall_quality_gate_passed=passed, full_feature_parity_established=False)
        return 0 if passed else 2
    except Exception as error:
        write_status(status, state="needs_diagnosis", reason=str(error), parity_established=False)
        print(str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
