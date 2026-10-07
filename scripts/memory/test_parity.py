import importlib.util
from pathlib import Path
import unittest
from dataclasses import dataclass

spec = importlib.util.spec_from_file_location("parity", Path(__file__).with_name("parity.py"))
parity = importlib.util.module_from_spec(spec)
spec.loader.exec_module(parity)


class ParityGateTests(unittest.TestCase):
    def test_duplicate_occurrences_preserve_content_and_timestamps(self):
        @dataclass
        class Document:
            id: str
            content: str
            timestamp: str
        sources = [Document("repeat", "same conversation", "2023-05-25"),
                   Document("repeat", "same conversation", "2023-05-30"),
                   Document("unique", "other conversation", "2023-06-01")]
        documents, expected, aliases = parity.unique_occurrences(sources, {"repeat"})
        self.assertEqual(len(documents), 3)
        self.assertEqual(len({d.id for d in documents}), 3)
        self.assertEqual([d.timestamp for d in documents], [d.timestamp for d in sources])
        self.assertEqual([d.content for d in documents], [d.content for d in sources])
        self.assertEqual(set(expected), set(aliases))
        self.assertEqual(sources[0].id, "repeat")
    def test_local_only_rejects_remote_and_credential_urls(self):
        for endpoint in ("http://127.0.0.1:1234/v1", "http://localhost:1234/v1", "http://[::1]:1234/v1"):
            parity.require_loopback(endpoint)
        for endpoint in ("https://openrouter.ai/api/v1", "http://127.0.0.1.example.com/v1", "http://user:pass@localhost/v1", "file:///tmp/model"):
            with self.assertRaises(ValueError):
                parity.require_loopback(endpoint)

    def test_resume_keeps_only_completed_questions_and_checks_profile(self):
        previous=self.report()
        previous['embedding_dimensions']='768'
        previous['runtime_evaluator_sha256']='old'
        current={'questions':[], 'embedding_dimensions':'768', 'runtime_evaluator_sha256':'old', 'reference_deployment_verified':False}
        previous['questions'][0]['error']='interrupted'
        self.assertEqual(len(parity.resumed_rows(previous,current,{str(i) for i in range(500)})),499)
        for key,value in [('embedding_dimensions','1536'),('runtime_evaluator_sha256','new')]:
            changed=dict(current,**{key:value})
            with self.assertRaises(ValueError):parity.resumed_rows(previous,changed,{str(i) for i in range(500)})
        with self.assertRaises(ValueError):parity.resumed_rows(previous,current,{'1'})
        previous['questions'].append(previous['questions'][1])
        with self.assertRaises(ValueError):parity.resumed_rows(previous,current,{str(i) for i in range(500)})

    def report(self):
        return {"history_mode": "full", "independent_judge": True, "reference_deployment_verified": True,
                "questions": [{"question_id": str(i), "category": parity.CATEGORIES[i % 6],
                               "backends": {name: {"judgment": {"correct": True}} for name in ("local", "hindsight")}} for i in range(500)],
                "answer_accuracy_difference": {"paired_bootstrap_95_percent": [-0.02, 0.03]}}

    def test_full_quality_pass_does_not_claim_missing_feature_parity(self):
        report = self.report()
        parity.gate(report)
        self.assertTrue(report["retain_recall_quality_gate"]["passed"])
        self.assertFalse(report["full_hindsight_parity"]["established"])
        self.assertIn("full upstream Reflect", report["full_hindsight_parity"]["missing_capabilities"])

    def test_pilot_oracle_errors_and_self_judge_cannot_pass(self):
        for modification in (
            {"history_mode": "oracle"}, {"independent_judge": False}, {"reference_deployment_verified": False},
            {"questions": [{"category": category} for category in parity.CATEGORIES]},
            {"questions": [{"category": "multi-session"} for _ in range(500)]},
            {"answer_accuracy_difference": {"paired_bootstrap_95_percent": [-0.06, 0.0]}},
        ):
            with self.subTest(modification=next(iter(modification))):
                report = self.report()
                report.update(modification)
                parity.gate(report)
                self.assertFalse(report["retain_recall_quality_gate"]["passed"])
        report = self.report()
        report["questions"][0]["error"] = "inference failed"
        parity.gate(report)
        self.assertFalse(report["retain_recall_quality_gate"]["passed"])

    def test_unfinished_question_cannot_pass_even_when_all_ids_are_present(self):
        report = self.report()
        del report["questions"][-1]["backends"]
        parity.gate(report)
        self.assertFalse(report["retain_recall_quality_gate"]["passed"])

    def test_paired_confidence_is_reproducible_and_penalizes_losses(self):
        self.assertEqual(parity.confidence([0, 1, -1, 0]), parity.confidence([0, 1, -1, 0]))
        self.assertEqual(parity.confidence([-1] * 10)["paired_bootstrap_95_percent"], [-1, -1])
        self.assertIsNone(parity.confidence([]))


if __name__ == "__main__":
    unittest.main()
