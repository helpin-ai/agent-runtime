import unittest
from local_pipeline import completed_stage


class PipelineTests(unittest.TestCase):
    def report(self):
        return {"local_only": True, "history_mode": "full", "reference_deployment_verified": True,
                "independent_judge": True, "questions": [{"question_id": "a", "backends": {
                    "local": {"judgment": {"correct": False}},
                    "hindsight": {"judgment": {"correct": True}}}}]}

    def test_completed_pilot_does_not_require_passing_full_gate(self):
        completed_stage(self.report(), 1)

    def test_no_expansion_from_failed_incomplete_or_oracle_cases(self):
        for field, value in (("local_only", False), ("history_mode", "oracle"),
                             ("reference_deployment_verified", False), ("independent_judge", False)):
            report = self.report(); report[field] = value
            with self.assertRaises(ValueError): completed_stage(report, 1)
        report = self.report(); report["questions"][0]["error"] = "ingestion failed"
        with self.assertRaises(ValueError): completed_stage(report, 1)
        report = self.report(); report["questions"][0]["backends"]["local"]["judgment"]["correct"] = "false"
        with self.assertRaises(ValueError): completed_stage(report, 1)
        with self.assertRaises(ValueError): completed_stage(self.report(), 6)


if __name__ == "__main__": unittest.main()
