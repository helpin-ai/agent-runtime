"""Select affected checks and reuse successful validation of an identical Git tree."""

import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import zipfile

from release_image_record import api, git_tree


CHECKS = {"ci": {"go", "react", "console", "manifests", "cross"}, "windows": {"windows"}}
WORKFLOWS = {"ci": ("ci.yml", "staging-release.yml", "production-release.yml"),
             "windows": ("windows.yml",)}


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def affected(kind, event):
    checks = CHECKS[kind].copy()
    pr = event.get("pull_request")
    if not pr:
        # Releases and direct pushes require all checks unless an identical tree was validated.
        if kind == "ci" and os.environ.get("GITHUB_EVENT_NAME") != "pull_request":
            checks.discard("console")  # The release image build validates the console.
        return checks
    try:
        paths = git("diff", "--name-only", "--no-renames", pr["base"]["sha"], "HEAD").splitlines()
    except (KeyError, subprocess.SubprocessError):
        return checks
    selected = set()
    for path in paths:
        if path.startswith(".github/workflows/") or path in (
                "scripts/ci_validation.py", "scripts/test_ci_validation.py"):
            return checks
        if path.startswith("packages/react/") or path == ".node-version":
            selected.add("react")
        if path.startswith("packages/console/") or path == ".node-version":
            selected.add("console")
        if path.startswith("scripts/") or path in ("Dockerfile", ".node-version"):
            selected.add("manifests")
        if path == ".node-version" or (path.startswith("scripts/") and path not in (
                "scripts/go-mod-download.sh", "scripts/windows-wsl-proof.ps1")):
            continue
        if path.startswith(("docs/", "packages/react/", "packages/console/", ".github/")):
            continue
        if "/" not in path and path.endswith(".md"):
            continue
        selected.update(("go", "cross", "windows"))
    return selected & checks


def trusted(run, repository):
    if (run.get("conclusion") != "success"
            or (run.get("head_repository") or {}).get("full_name") != repository):
        return False
    if run.get("event") in ("push", "workflow_dispatch"):
        return run.get("head_branch") in ("develop", "main")
    prs = run.get("pull_requests") or []
    return (run.get("event") == "pull_request" and len(prs) == 1
            and prs[0].get("base", {}).get("ref") in ("develop", "main")
            and prs[0].get("head", {}).get("repo", {}).get("id")
            == prs[0].get("base", {}).get("repo", {}).get("id")
            and prs[0].get("head", {}).get("repo", {}).get("id") is not None)


def valid_record(record, run, tree, kind):
    return (isinstance(record, dict) and record.get("schema") == 1
            and record.get("kind") == kind and record.get("tree") == tree
            and record.get("head_sha") == run["head_sha"]
            and record.get("run_id") == str(run["id"])
            and record.get("run_attempt") == str(run["run_attempt"])
            and isinstance(record.get("commit"), str)
            and re.fullmatch(r"[0-9a-f]{40}", record["commit"]) is not None
            and isinstance(record.get("checks"), list)
            and all(isinstance(check, str) and check in CHECKS[kind] for check in record["checks"]))


def find_verified(kind, tree, required):
    repository = os.environ["GITHUB_REPOSITORY"]
    verified = set()
    # shortcut: inspect the latest 20 records for this exact tree; expired records rerun checks.
    artifacts = api(f"repos/{repository}/actions/artifacts?name=verified-{kind}-{tree}&per_page=20")["artifacts"]
    for artifact in artifacts:
        if verified >= required:
            break
        if artifact.get("name") != f"verified-{kind}-{tree}" or artifact.get("expired"):
            continue
        try:
            run = api(f"repos/{repository}/actions/runs/{artifact['workflow_run']['id']}")
            if (run.get("path") not in {".github/workflows/" + name for name in WORKFLOWS[kind]}
                    or not trusted(run, repository)):
                continue
            archive = api(f"repos/{repository}/actions/artifacts/{artifact['id']}/zip", binary=True)
            with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
                if bundle.getinfo("validation.json").file_size > 16 * 1024:
                    continue
                record = json.loads(bundle.read("validation.json"))
            if not valid_record(record, run, tree, kind):
                continue
            commit = api(f"repos/{repository}/git/commits/{record['commit']}")
            if commit["tree"]["sha"] != tree:
                continue
            # PRs check out the synthetic merge commit, not necessarily the run's head SHA.
            if record["commit"] != run["head_sha"] and not any(
                    parent["sha"] == run["head_sha"] for parent in commit["parents"]):
                continue
            verified.update(record["checks"])
            print(f"Reusing {sorted(set(record['checks']) & required)} from run {run['id']}")
        except (OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError, zipfile.BadZipFile) as error:
            print(f"Cannot reuse validation artifact {artifact['id']}: {error}", file=sys.stderr)
    return verified


def plan(kind):
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    required = affected(kind, event)
    tree = git_tree("HEAD")
    reused = set()
    if required:
        try:
            reused = find_verified(kind, tree, required) & required
        except (OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError) as error:
            print(f"Validation lookup unavailable; running checks: {error}", file=sys.stderr)
    result = {"required": sorted(required), "reused": sorted(reused)}
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write("plan=" + json.dumps(result, separators=(",", ":")) + "\n")
        for check in sorted(CHECKS[kind]):
            output.write(f"{check}={str(check in required - reused).lower()}\n")
    print(json.dumps(result))


def record(kind):
    planned = json.loads(os.environ["PLAN_JSON"])
    results = json.loads(os.environ["RESULTS_JSON"])
    required, reused = set(planned["required"]), set(planned["reused"])
    if not reused <= required <= CHECKS[kind]:
        raise ValueError("invalid validation plan")
    for check in required - reused:
        if results[check]["result"] != "success":
            raise ValueError(f"required check did not pass: {check}")
    event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    result = {"schema": 1, "kind": kind, "tree": git_tree("HEAD"), "commit": git("rev-parse", "HEAD"),
              "head_sha": event.get("pull_request", {}).get("head", {}).get("sha", os.environ["GITHUB_SHA"]),
              "run_id": os.environ["GITHUB_RUN_ID"], "run_attempt": os.environ["GITHUB_RUN_ATTEMPT"],
              "checks": sorted(required)}
    Path("validation.json").write_text(json.dumps(result) + "\n")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write("tree=" + result["tree"] + "\n")


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] not in ("plan", "record") or sys.argv[2] not in CHECKS:
        raise SystemExit("usage: ci_validation.py plan|record ci|windows")
    {"plan": plan, "record": record}[sys.argv[1]](sys.argv[2])
