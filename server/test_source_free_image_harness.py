"""Source-free receipt validation and owned-resource cleanup regression checks."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

import test_source_free_image as harness


class SourceFreeImageHarnessTest(unittest.TestCase):
    def test_receipt_rejects_extra_body_and_invalid_boundary(self):
        evidence = {"probe": "synthetic-dynamic/1", "outcome": "pass", "publicAdmission": False,
                    "networkCalls": 0, "syntheticReads": 1, "evidenceSha256": "a" * 64}
        self.assertEqual(evidence, harness.checked_metadata("synthetic_dynamic", evidence))
        for mutation in ({"rows": ["SYNTHETIC_MARKER"]}, {"networkCalls": 1}, {"publicAdmission": True}, {"syntheticReads": True}):
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                harness.checked_metadata("synthetic_dynamic", dict(evidence, **mutation))

    def test_default_image_checks_are_network_isolated_and_never_claim_tls(self):
        self.exercise(cleanup_ok=True)

    def test_cleanup_failure_cannot_pass(self):
        self.exercise(cleanup_ok=False)

    def exercise(self, cleanup_ok):
        commands = []

        def command(arguments, **options):
            commands.append(arguments)
            code, output = 0, ""
            if arguments[:2] == ["go", "build"]:
                Path(arguments[arguments.index("-o") + 1]).write_bytes(b"synthetic binary")
            elif arguments[:3] == ["docker", "image", "inspect"]:
                output = "sha256:" + "b" * 64
            elif arguments[:2] == ["docker", "run"]:
                self.assertEqual("none", arguments[arguments.index("--network") + 1])
                self.assertIn("--read-only", arguments)
                self.assertNotIn("--rm", arguments)
                if arguments[-1] == "--roots-probe":
                    output = json.dumps({"probe": "roots/1", "outcome": "pass", "rootsSha256": "c" * 64, "certificates": 1})
                else:
                    self.assertEqual("--synthetic-dynamic-probe", arguments[-1])
                    output = json.dumps({"probe": "synthetic-dynamic/1", "outcome": "pass", "publicAdmission": False,
                                         "networkCalls": 0, "syntheticReads": 1, "evidenceSha256": "a" * 64})
            elif arguments[:3] == ["docker", "image", "rm"] and not cleanup_ok:
                code = 1
            return subprocess.CompletedProcess(arguments, code, output, "")

        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary) / "report.json"
            with mock.patch.object(harness, "run", side_effect=command), mock.patch("sys.argv", ["probe", "--report", str(report)]):
                result = harness.main()
            receipt = json.loads(report.read_text())
        self.assertEqual(0 if cleanup_ok else 1, result)
        self.assertEqual(cleanup_ok, receipt["cleanup_complete"])
        self.assertFalse(receipt["shipping_public_root_tls_verified"])
        self.assertFalse(receipt["public_dynamic_inventory_admitted"])
        self.assertFalse(receipt["deployed_chain_cancellation_verified"])
        removals = [args for args in commands if args[:3] == ["docker", "rm", "-f"]]
        self.assertEqual(2, len(removals))
        self.assertEqual(set(receipt["owned_containers"]), {args[3] for args in removals})
