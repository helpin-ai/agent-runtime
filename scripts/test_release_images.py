"""Release orchestration regression tests; Docker is mocked, no registry writes."""

import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parent.parent
IMAGES = ["ghcr.io/helpin-ai/agent-runtime" + suffix for suffix in ("", "-coding", "-console")]
DIGESTS = ["sha256:" + digit * 64 for digit in "123"]


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
