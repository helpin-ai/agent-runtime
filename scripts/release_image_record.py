"""Record tested image digests and find a matching successful staging build."""

import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import zipfile


IMAGES = {name: "ghcr.io/helpin-ai/agent-runtime" + suffix for name, suffix in
          (("runtime", ""), ("coding", "-coding"), ("console", "-console"))}
ARTIFACT = "tested-release-images"
RECORD = "release-images.json"
PLATFORM = "linux/amd64"


def git_tree(commit):
    return subprocess.check_output(["git", "rev-parse", commit + "^{tree}"], text=True).strip()


def valid_record(record, run, tree):
    return (isinstance(record, dict) and record.get("schema") == 1 and record.get("commit") == run["head_sha"]
            and record.get("tree") == tree and record.get("platform") == PLATFORM
            and record.get("run_id") == str(run["id"])
            and record.get("run_attempt") == str(run["run_attempt"])
            and isinstance(record.get("images"), dict)
            and set(record["images"]) == set(IMAGES)
            and all(isinstance(digest, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", digest)
                    for digest in record["images"].values()))


def api(path, binary=False):
    data = subprocess.check_output(["gh", "api", path], timeout=60)
    return data if binary else json.loads(data)


def find_record(repository, commit):
    tree = git_tree(commit)
    # shortcut: inspect the last 100 successful staging runs; widen if release gaps exceed this.
    runs = api(f"repos/{repository}/actions/workflows/staging-release.yml/runs"
               "?branch=develop&status=success&per_page=100")["workflow_runs"]
    deadline = time.monotonic() + 180
    for run in runs:
        if time.monotonic() > deadline:
            break
        if (run.get("conclusion") != "success" or run.get("head_branch") != "develop"
                or run.get("event") not in ("push", "workflow_dispatch")
                or (run.get("head_repository") or {}).get("full_name") != repository):
            continue
        try:
            if git_tree(run["head_sha"]) != tree:
                continue
            artifacts = api(f"repos/{repository}/actions/runs/{run['id']}/artifacts")["artifacts"]
            matches = [a for a in artifacts if a["name"] == ARTIFACT and not a["expired"]]
            if len(matches) != 1:
                continue
            archive = api(f"repos/{repository}/actions/artifacts/{matches[0]['id']}/zip", binary=True)
            with zipfile.ZipFile(io.BytesIO(archive)) as bundle:
                if bundle.getinfo(RECORD).file_size > 64 * 1024:
                    continue
                record = json.loads(bundle.read(RECORD))
            if not valid_record(record, run, tree):
                continue
            # Inspect manifests only. Missing registry images must trigger a rebuild.
            for name, image in IMAGES.items():
                subprocess.run(["docker", "manifest", "inspect", image + "@" + record["images"][name]],
                               check=True, stdout=subprocess.DEVNULL, timeout=60)
            print(f"Reusing tested staging images from run {run['id']} ({run['head_sha']})")
            return record
        except (OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError, zipfile.BadZipFile) as error:
            print(f"Cannot reuse staging run {run['id']}: {error}", file=sys.stderr)
    return None


def main():
    if sys.argv[1:] == ["record"]:
        record = {"schema": 1, "commit": os.environ["GITHUB_SHA"],
                  "tree": git_tree(os.environ["GITHUB_SHA"]), "platform": PLATFORM,
                  "run_id": os.environ["GITHUB_RUN_ID"], "run_attempt": os.environ["GITHUB_RUN_ATTEMPT"],
                  "images": {name: os.environ[name.upper() + "_DIGEST"] for name in IMAGES}}
        run = {"head_sha": record["commit"], "id": record["run_id"], "run_attempt": record["run_attempt"]}
        if not valid_record(record, run, record["tree"]):
            raise ValueError("invalid tested-image record")
        Path(RECORD).write_text(json.dumps(record) + "\n")
    elif sys.argv[1:] == ["find"]:
        record = None
        try:
            record = find_record(os.environ["GITHUB_REPOSITORY"], os.environ["GITHUB_SHA"])
        except (OSError, subprocess.SubprocessError, ValueError, KeyError, TypeError) as error:
            print(f"Staging lookup unavailable; building instead: {error}", file=sys.stderr)
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"reuse={'true' if record else 'false'}\n")
            if record:
                for name, digest in record["images"].items():
                    output.write(f"{name}_digest={digest}\n")
    else:
        raise SystemExit("usage: release_image_record.py record|find")


if __name__ == "__main__":
    main()
