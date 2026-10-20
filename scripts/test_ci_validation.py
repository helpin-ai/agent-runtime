"""Validation selection and provenance tests; no GitHub writes or network calls."""

import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import ci_validation as validation


class SelectionTests(unittest.TestCase):
    def selected(self, kind, paths):
        with patch.object(validation, "git", return_value="\n".join(paths)):
            return validation.affected(kind, {"pull_request": {"base": {"sha": "base"}}})

    def test_changed_areas_select_only_relevant_checks(self):
        for paths, expected in (
            (["docs/memory.md", "README.md"], set()),
            (["packages/console/src/index.ts"], {"console"}),
            (["packages/react/src/index.test.ts"], {"react"}),
            ([".node-version"], {"react", "console", "manifests"}),
            (["scripts/release_image_record.py"], {"manifests"}),
            (["internal/tools/browser_extension/content.js"], {"go", "cross"}),
            (["internal/tools/testdata/prompt.md"], {"go", "cross"}),
            (["sandbox/wsl/client_windows.go"], {"go", "cross"}),
            (["go.sum"], {"go", "cross"}),
            (["Dockerfile"], {"go", "cross", "manifests"}),
            (["packages/new-package/index.js"], {"go", "cross"}),
            (["scripts/ci_validation.py"], validation.CHECKS["ci"]),
            ([".github/workflows/ci.yml"], validation.CHECKS["ci"]),
        ):
            with self.subTest(paths=paths):
                self.assertEqual(self.selected("ci", paths), expected)
        for path in ("docs/memory.md", "packages/console/package.json", "scripts/test_release_images.py"):
            self.assertEqual(self.selected("windows", [path]), set())
        for path in ("go.mod", "internal/filelock/lock_windows.go", ".github/workflows/windows.yml"):
            self.assertEqual(self.selected("windows", [path]), {"windows"})

    def test_releases_and_direct_pushes_do_not_skip_untested_changes(self):
        with patch.dict(os.environ, GITHUB_EVENT_NAME="push"):
            self.assertEqual(validation.affected("ci", {}), validation.CHECKS["ci"] - {"console"})
        self.assertEqual(validation.affected("windows", {}), {"windows"})
        with patch.object(validation, "git", side_effect=subprocess.CalledProcessError(1, "git")):
            self.assertEqual(validation.affected("ci", {"pull_request": {"base": {"sha": "missing"}}}),
                             validation.CHECKS["ci"])


class ReuseTests(unittest.TestCase):
    def setUp(self):
        self.tree = "c" * 40
        self.head = "a" * 40
        self.commit = "b" * 40
        self.run = {"id": 7, "run_attempt": 2, "head_sha": self.head, "conclusion": "success",
                    "path": ".github/workflows/ci.yml", "event": "pull_request",
                    "head_repository": {"full_name": "helpin-ai/agent-runtime"},
                    "pull_requests": [{"head": {"repo": {"id": 1}},
                                       "base": {"ref": "develop", "repo": {"id": 1}}}]}
        self.record = {"schema": 1, "kind": "ci", "tree": self.tree, "commit": self.commit,
                       "head_sha": self.head, "run_id": "7", "run_attempt": "2", "checks": ["go", "cross"]}
        self.artifact = {"id": 11, "name": "verified-ci-" + self.tree, "expired": False,
                         "workflow_run": {"id": 7}}

    def find(self, record=None, run=None, artifacts=None, tree=None, parents=None, raw=None, kind="ci"):
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as bundle:
            bundle.writestr("validation.json", raw if raw is not None else json.dumps(
                self.record if record is None else record))

        def api(path, binary=False):
            if "/actions/artifacts?" in path:
                return {"artifacts": [self.artifact] if artifacts is None else artifacts}
            if path.endswith("/runs/7"):
                return self.run if run is None else run
            if binary:
                self.assertEqual(path, "repos/helpin-ai/agent-runtime/actions/artifacts/11/zip")
                return archive.getvalue()
            self.assertEqual(path, "repos/helpin-ai/agent-runtime/git/commits/" + self.commit)
            return {"tree": {"sha": self.tree if tree is None else tree},
                    "parents": [{"sha": self.head}] if parents is None else parents}

        with patch.dict(os.environ, GITHUB_REPOSITORY="helpin-ai/agent-runtime"), \
                patch.object(validation, "api", side_effect=api):
            return validation.find_verified(kind, self.tree, validation.CHECKS[kind])

    def test_identical_merge_tree_reuses_only_recorded_checks(self):
        self.assertEqual(self.find(), {"go", "cross"})
        self.assertEqual(self.find(record=dict(self.record, checks=["go"])), {"go"})
        for branch in ("develop", "main"):
            run = dict(self.run, event="push", head_branch=branch, path=".github/workflows/staging-release.yml")
            self.assertEqual(self.find(run=run), {"go", "cross"})

    def test_rejects_failed_foreign_fork_and_wrong_workflow_runs(self):
        for changes in ({"conclusion": "failure"}, {"event": "pull_request_target"},
                        {"head_repository": {"full_name": "fork/agent-runtime"}},
                        {"path": ".github/workflows/other.yml"},
                        {"event": "push", "head_branch": "feature"}, {"pull_requests": []},
                        {"pull_requests": [{"head": {"repo": {"id": 2}},
                                            "base": {"ref": "develop", "repo": {"id": 1}}}]},
                        {"pull_requests": [{"head": {"repo": {"id": 1}},
                                            "base": {"ref": "feature", "repo": {"id": 1}}}]}):
            with self.subTest(changes=changes):
                self.assertEqual(self.find(run=dict(self.run, **changes)), set())

    def test_rejects_wrong_provenance_or_invented_checks(self):
        for changes in ({"schema": 2}, {"kind": "windows"}, {"tree": "d" * 40},
                        {"head_sha": self.commit}, {"commit": "bad"}, {"run_id": "8"},
                        {"run_attempt": "1"}, {"checks": ["invented"]}, {"checks": "go"},
                        {"checks": [{}]}):
            with self.subTest(changes=changes):
                self.assertEqual(self.find(record=dict(self.record, **changes)), set())
        self.assertEqual(self.find(tree="d" * 40), set())
        self.assertEqual(self.find(parents=[]), set())
        self.assertEqual(self.find(raw="["), set())
        self.assertEqual(self.find(raw=" " * (16 * 1024 + 1)), set())

    def test_missing_expired_or_wrong_tree_artifact_reruns(self):
        self.assertEqual(self.find(artifacts=[]), set())
        self.assertEqual(self.find(artifacts=[dict(self.artifact, expired=True)]), set())
        self.assertEqual(self.find(artifacts=[dict(self.artifact, name="verified-ci-other")]), set())

    def test_windows_validation_has_its_own_workflow_and_artifact(self):
        record = dict(self.record, kind="windows", checks=["windows"])
        artifact = dict(self.artifact, name="verified-windows-" + self.tree)
        run = dict(self.run, path=".github/workflows/windows.yml")
        self.assertEqual(self.find(record=record, run=run, artifacts=[artifact], kind="windows"), {"windows"})
        self.assertEqual(self.find(record=record, run=run, kind="windows"), set())


class WorkflowStateTests(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.addCleanup(self.folder.cleanup)
        self.root = Path(self.folder.name)
        event = self.root / "event.json"
        event.write_text(json.dumps({"pull_request": {"head": {"sha": "a" * 40}}}))
        self.env = dict(GITHUB_EVENT_PATH=str(event), GITHUB_OUTPUT=str(self.root / "outputs"),
                        GITHUB_SHA="b" * 40, GITHUB_RUN_ID="7", GITHUB_RUN_ATTEMPT="2")

    def test_api_failure_falls_back_to_running_required_checks(self):
        with patch.dict(os.environ, self.env), patch.object(validation, "affected", return_value={"go", "cross"}), \
                patch.object(validation, "git_tree", return_value="c" * 40), \
                patch.object(validation, "find_verified", side_effect=subprocess.CalledProcessError(1, "gh")):
            validation.plan("ci")
        outputs = (self.root / "outputs").read_text()
        self.assertIn("go=true\n", outputs)
        self.assertIn("cross=true\n", outputs)
        self.assertIn("react=false\n", outputs)

    def test_matching_record_skips_only_the_certified_checks(self):
        with patch.dict(os.environ, self.env), patch.object(validation, "affected", return_value={"go", "cross"}), \
                patch.object(validation, "git_tree", return_value="c" * 40), \
                patch.object(validation, "find_verified", return_value={"go", "react"}):
            validation.plan("ci")
        outputs = (self.root / "outputs").read_text()
        self.assertIn("go=false\n", outputs)
        self.assertIn("cross=true\n", outputs)
        self.assertIn('"reused":["go"]', outputs)

    def record(self, planned, results):
        with patch.dict(os.environ, dict(self.env, PLAN_JSON=json.dumps(planned), RESULTS_JSON=json.dumps(results))), \
                patch.object(validation, "git_tree", return_value="c" * 40), \
                patch.object(validation, "git", return_value="b" * 40), \
                patch.object(validation.Path, "write_text") as write:
            validation.record("ci")
            return json.loads(write.call_args.args[0])

    def test_failed_cancelled_missing_or_unexpectedly_skipped_checks_block_record(self):
        planned = {"required": ["go"], "reused": []}
        for result in ("failure", "cancelled", "skipped"):
            with self.assertRaises(ValueError):
                self.record(planned, {"go": {"result": result}})
        with self.assertRaises(KeyError):
            self.record(planned, {})
        with self.assertRaises(ValueError):
            self.record({"required": ["invented"], "reused": ["invented"]}, {})

    def test_only_successful_and_reused_checks_are_certified(self):
        record = self.record({"required": ["go", "cross"], "reused": ["cross"]},
                             {"go": {"result": "success"}, "cross": {"result": "skipped"}})
        self.assertEqual(record["checks"], ["cross", "go"])
        self.assertEqual(record["head_sha"], "a" * 40)
        self.assertEqual(record["commit"], "b" * 40)
        self.assertEqual((self.root / "outputs").read_text(), "tree=" + "c" * 40 + "\n")


if __name__ == "__main__":
    unittest.main()
