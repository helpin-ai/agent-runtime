from pathlib import Path
import tempfile
import unittest

from budget_proxy import Ledger


class LedgerTests(unittest.TestCase):
    def test_inflight_reservations_cannot_oversubscribe_budget(self):
        with tempfile.TemporaryDirectory() as tmp:
            ledger = Ledger(0.1, Path(tmp)/"audit.json")
            self.assertTrue(ledger.reserve(0.07))
            self.assertFalse(ledger.reserve(0.04))
            ledger.complete(0.07, "model", 200, {"cost":0.01, "prompt_tokens":10})
            self.assertEqual(ledger.spent, 0.01)
            self.assertEqual(ledger.reserved, 0)
            self.assertTrue(ledger.reserve(0.08))
            self.assertFalse(ledger.reserve(0.02))

    def test_missing_usage_is_charged_conservatively(self):
        with tempfile.TemporaryDirectory() as tmp:
            ledger = Ledger(0.1, Path(tmp)/"audit.json")
            self.assertTrue(ledger.reserve(0.08))
            ledger.complete(0.08, "model", 502, None)
            self.assertEqual(ledger.spent, 0.08)
            self.assertFalse(ledger.reserve(0.03))

    def test_nonfinite_or_negative_limits_rejected(self):
        for limit in (0, -1, float("nan"), float("inf")):
            with self.subTest(limit=limit):
                with self.assertRaises(ValueError):
                    Ledger(limit, Path("unused-audit.json"))


if __name__ == "__main__":
    unittest.main()
