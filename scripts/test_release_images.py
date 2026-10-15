"""Release orchestration regression tests; Docker is mocked, no registry writes."""

import os
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import release_image_record as records


ROOT = Path(__file__).resolve().parent.parent
IMAGES = ["ghcr.io/helpin-ai/agent-runtime" + suffix for suffix in ("", "-coding", "-console")]
DIGESTS = ["sha256:" + digit * 64 for digit in "123"]


class StagingReuseTests(unittest.TestCase):
    def setUp(self):
        self.repository = "helpin-ai/agent-runtime"
        self.tree = "c" * 40
        self.commit = "a" * 40
        self.run = {"id": 7, "run_attempt": 2, "head_sha": "b" * 40,
                    "conclusion": "success", "head_branch": "develop", "event": "push",
                    "head_repository": {"full_name": self.repository}}
        self.record = {"schema": 1, "tree": self.tree, "commit": self.run["head_sha"],
                       "run_id": "7", "run_attempt": "2", "platform": "linux/amd64",
                       "images": dict(zip(records.IMAGES, DIGESTS))}

    def find(self, record=None, run=None, artifacts=None, image_missing=False, source_tree=None):
        archive = io.BytesIO()
        with zipfile.ZipFile(archive, "w") as bundle:
            bundle.writestr(records.RECORD, json.dumps(self.record if record is None else record))
        if artifacts is None:
            artifacts = [{"id": 11, "name": records.ARTIFACT, "expired": False}]

        def api(path, binary=False):
            if "/workflows/" in path:
                return {"workflow_runs": [self.run if run is None else run]}
            if path.endswith("/artifacts"):
                return {"artifacts": artifacts}
            self.assertTrue(binary)
            self.assertEqual(path, f"repos/{self.repository}/actions/artifacts/11/zip")
            return archive.getvalue()

        with patch.object(records, "api", side_effect=api), \
                patch.object(records, "git_tree", side_effect=lambda commit: self.tree if commit == self.commit else source_tree or self.tree), \
                patch.object(records.subprocess, "run") as inspect:
            if image_missing:
                inspect.side_effect = subprocess.CalledProcessError(1, "docker")
            result = records.find_record(self.repository, self.commit)
            return result, inspect.call_args_list

    def test_identical_tree_with_different_merge_commit_reuses_exact_digests(self):
        result, inspections = self.find()
        self.assertEqual(result, self.record)
        self.assertEqual(len(inspections), 3)
        for call, image, digest in zip(inspections, IMAGES, DIGESTS):
            self.assertEqual(call.args[0], ["docker", "manifest", "inspect", image + "@" + digest])

    def test_source_difference_rebuilds(self):
        result, inspections = self.find(source_tree="d" * 40)
        self.assertIsNone(result)
        self.assertEqual(inspections, [])

    def test_rejects_unverified_or_foreign_workflows(self):
        for changes in ({"conclusion": "failure"}, {"head_branch": "feature"},
                        {"event": "pull_request"}, {"head_repository": {"full_name": "other/repo"}}):
            with self.subTest(changes=changes):
                result, inspections = self.find(run=dict(self.run, **changes))
                self.assertIsNone(result)
                self.assertEqual(inspections, [])

    def test_missing_or_expired_artifacts_rebuild(self):
        for artifacts in ([], [{"id": 11, "name": records.ARTIFACT, "expired": True}]):
            result, inspections = self.find(artifacts=artifacts)
            self.assertIsNone(result)
            self.assertEqual(inspections, [])

    def test_invalid_metadata_rebuilds_without_registry_access(self):
        for changes in ({"schema": 2}, {"commit": self.commit}, {"tree": "d" * 40},
                        {"run_id": "8"}, {"run_attempt": "1"}, {"platform": "linux/arm64"},
                        {"images": {}}, {"images": dict(self.record["images"], coding="candidate-7")},
                        {"images": dict(self.record["images"], unexpected=DIGESTS[0])}):
            with self.subTest(changes=changes):
                result, inspections = self.find(record=dict(self.record, **changes))
                self.assertIsNone(result)
                self.assertEqual(inspections, [])

    def test_missing_registry_digest_rebuilds(self):
        result, _ = self.find(image_missing=True)
        self.assertIsNone(result)

    def test_lookup_failure_emits_build_fallback(self):
        with tempfile.TemporaryDirectory() as folder:
            output = Path(folder) / "outputs"
            with patch.dict(os.environ, GITHUB_REPOSITORY=self.repository, GITHUB_SHA=self.commit, GITHUB_OUTPUT=str(output)), \
                    patch.object(records.sys, "argv", ["script", "find"]), \
                    patch.object(records, "find_record", side_effect=subprocess.CalledProcessError(1, "gh")):
                records.main()
            self.assertEqual(output.read_text(), "reuse=false\n")

    def test_record_and_selected_outputs_preserve_digests(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / records.RECORD
            output = Path(folder) / "outputs"
            env = dict(GITHUB_SHA=self.run["head_sha"], GITHUB_RUN_ID="7", GITHUB_RUN_ATTEMPT="2",
                       GITHUB_REPOSITORY=self.repository, GITHUB_OUTPUT=str(output),
                       **{name.upper() + "_DIGEST": digest for name, digest in self.record["images"].items()})
            with patch.dict(os.environ, env), patch.object(records, "git_tree", return_value=self.tree), \
                    patch.object(records, "RECORD", str(path)), patch.object(records.sys, "argv", ["script", "record"]):
                records.main()
            self.assertEqual(json.loads(path.read_text()), self.record)
            with patch.dict(os.environ, env), patch.object(records.sys, "argv", ["script", "find"]), \
                    patch.object(records, "find_record", return_value=self.record):
                records.main()
            self.assertEqual(output.read_text().splitlines(), ["reuse=true"] +
                             [f"{name}_digest={digest}" for name, digest in self.record["images"].items()])


class PromotionTests(unittest.TestCase):
    def promote(self, pairs=None, **overrides):
        env = dict(os.environ, VERSION="v1.2.3", CHANNEL="stage", GITHUB_SHA="a" * 40,
                   MOCK_FAIL_AT="0")
        env.update(overrides)
        if pairs is None:
            pairs = [part for pair in zip(IMAGES, DIGESTS) for part in pair]
        return subprocess.run(
            ["bash", "-c", r'''
calls=0
docker() {
  calls=$((calls + 1))
  printf '%s\t' "$@"
  printf '\n'
  if [[ "$calls" == "$MOCK_FAIL_AT" ]]; then return 37; fi
}
source scripts/promote-release-images.sh "$@"
''', "test", *pairs], cwd=ROOT, env=env, capture_output=True, text=True,
        )

    def test_promotes_exact_digests_before_advancing_aliases(self):
        result = self.promote()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [line.rstrip("\t").split("\t") for line in result.stdout.splitlines()]
        self.assertEqual(len(calls), 6)
        for index, (image, digest) in enumerate(zip(IMAGES, DIGESTS)):
            self.assertEqual(calls[index], [
                "buildx", "imagetools", "create", "--prefer-index=false",
                "--tag", image + ":v1.2.3", "--tag", image + ":sha-" + "a" * 40,
                image + "@" + digest,
            ])
            self.assertEqual(calls[index + 3], [
                "buildx", "imagetools", "create", "--prefer-index=false",
                "--tag", image + ":stage-latest", image + "@" + digest,
            ])

    def test_production_channel(self):
        result = self.promote(CHANNEL="prod")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.count(":prod-latest"), 3)

    def test_validates_all_inputs_before_publishing_any_image(self):
        for pairs in (
            [], [IMAGES[0]],
            [IMAGES[0], DIGESTS[0], IMAGES[1], ""],
            [IMAGES[0], DIGESTS[0], IMAGES[1], "candidate-123-1"],
            [IMAGES[0], DIGESTS[0], "ghcr.io/other/image", DIGESTS[1]],
        ):
            with self.subTest(pairs=pairs):
                result = self.promote(pairs)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")

    def test_rejects_invalid_release_metadata(self):
        for overrides in ({"VERSION": ""}, {"VERSION": "bad tag"},
                          {"CHANNEL": "unknown"}, {"GITHUB_SHA": ""}):
            with self.subTest(overrides=overrides):
                result = self.promote(**overrides)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")

    def test_publish_failure_does_not_advance_aliases(self):
        result = self.promote(MOCK_FAIL_AT="2")
        self.assertEqual(result.returncode, 37)
        self.assertEqual(len(result.stdout.splitlines()), 2)
        self.assertNotIn("stage-latest", result.stdout)


class ConsoleSmokeTests(unittest.TestCase):
    def smoke(self, healthy):
        return subprocess.run(["bash", "-c", r'''
docker() {
  case "$1" in
    run) echo test-container ;;
    exec) [[ "$HEALTHY" == yes ]] ;;
    logs) echo diagnostic-logs ;;
    rm) printf 'cleanup %s\n' "$*" >&2 ;;
    *) return 99 ;;
  esac
}
sleep() { :; }
source scripts/container-console-smoke.sh test-image
'''], cwd=ROOT, env=dict(os.environ, HEALTHY=healthy), capture_output=True, text=True)

    def test_healthy_container_is_cleaned_up(self):
        result = self.smoke("yes")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("cleanup rm --force test-container", result.stderr)

    def test_failed_health_check_blocks_release_and_cleans_up(self):
        result = self.smoke("no")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("diagnostic-logs", result.stdout)
        self.assertIn("console health check failed", result.stderr)
        self.assertIn("cleanup rm --force test-container", result.stderr)


if __name__ == "__main__":
    unittest.main()
