"""Offline adversarial harness checks: no Docker daemon, ports or network."""
import contextlib
import io
import json
from pathlib import Path
import signal
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

import prepare_fixture
import test_actual_runtime as actual


class ActualHarnessTest(unittest.TestCase):
    def test_deadline_reserves_cleanup_and_refuses_insufficient_real_expiry(self):
        commands = actual.Commands({"commands": []}, 1000)
        with patch.object(actual.time, "monotonic", return_value=500):
            self.assertEqual(440, commands.budget(530, minimum=345))
        with patch.object(actual.time, "monotonic", return_value=620):
            with self.assertRaisesRegex(RuntimeError, "insufficient deadline"):
                commands.budget(530, minimum=345)
        with patch.object(actual.time, "monotonic", return_value=970):
            self.assertEqual(15, commands.budget(15, cleanup=True))

    def test_process_timeout_kills_owned_group_and_records_actual_exit(self):
        receipt = {"commands": []}
        process = Mock(pid=1234)
        process.wait.side_effect = [subprocess.TimeoutExpired("docker", 1), -9]
        with patch.object(actual.subprocess, "Popen", return_value=process), patch.object(actual.os, "killpg") as kill, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "timed out"):
                actual.Commands(receipt, time.monotonic()+100).run(["docker", "run", "owned"], timeout=1)
        kill.assert_called_once_with(1234, signal.SIGKILL)
        self.assertEqual(-9, receipt["commands"][0]["exit"])
        self.assertEqual("failed", receipt["commands"][0]["outcome"])

    def test_nonzero_exit_cannot_be_overwritten_by_later_proxy(self):
        receipt = {"commands": []}
        process = Mock()
        process.wait.return_value = 7
        with patch.object(actual.subprocess, "Popen", return_value=process), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "phase exit 7"):
                actual.Commands(receipt, time.monotonic()+100).run(["python3", "generator", "--check"])
        self.assertEqual(7, receipt["commands"][0]["exit"])
        self.assertEqual("failed", receipt["commands"][0]["outcome"])

    def test_cleanup_retains_original_failure_and_every_owned_identity(self):
        receipt = {"outcome": "hosted_linux_candidate_pass", "primary_error": "original failure"}
        commands = Mock()
        commands.run.side_effect = subprocess.TimeoutExpired("docker", 2)
        containers = ["owned-production", "owned-workload"]
        actual.cleanup_owned(commands, containers, ["owned:image"], receipt)
        self.assertEqual("failed", receipt["outcome"])
        self.assertEqual("original failure", receipt["primary_error"])
        self.assertEqual(containers, receipt["remaining_containers"])
        self.assertEqual(3, len(receipt["cleanup_errors"]))
        self.assertFalse(receipt["cleanup_complete"])

    def test_failed_main_always_writes_receipt_when_cleanup_also_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary)/"report.json"
            def failure(_commands, _directory, _receipt, _prefix, containers, images):
                containers.append("owned-timeout")
                images.append("owned:image")
                raise RuntimeError("primary production timeout")
            with patch.dict(actual.os.environ, {"GITHUB_ACTIONS":"false", "OVDB_ACTUAL_EXPECTED_SOURCE":"", "OVDB_ACTUAL_EVENT":""}), patch.object(actual.sys, "argv", ["test", "--report", str(report)]), patch.object(actual, "experiment", side_effect=failure), patch.object(actual.Commands, "run", side_effect=["source-head", RuntimeError("inspect failed"), RuntimeError("logs failed"), RuntimeError("cleanup failed"), RuntimeError("image cleanup failed")]), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(1, actual.main())
            receipt = json.loads(report.read_bytes())
            self.assertEqual("primary production timeout", receipt["primary_error"])
            self.assertEqual("failed", receipt["outcome"])
            self.assertEqual(["owned-timeout"], receipt["remaining_containers"])
            self.assertEqual(["owned:image"], receipt["owned_images"])
            self.assertFalse(receipt["cleanup_complete"])

    def test_ballast_is_only_missing_natural_envelope(self):
        self.assertEqual(28<<20, actual.ballast_bytes(100<<20))
        self.assertEqual(0, actual.ballast_bytes(128<<20))
        for invalid in (-1, (128<<20)+1, True, 1.5):
            with self.assertRaises(ValueError):
                actual.ballast_bytes(invalid)

    def test_go_receipt_rejects_skipped_failed_or_repeated_evidence(self):
        passed = 'ACTUAL_CAPACITY_JSON={"outcome":"passed"}\n--- PASS: TestActualCapacity (1.00s)\n'
        self.assertEqual("passed", actual.parse_go_receipt(passed, "ACTUAL_CAPACITY_JSON=")["outcome"])
        for output in (passed+passed, passed+'--- SKIP: test\n', 'ACTUAL_CAPACITY_JSON={"outcome":"failed"}\n', 'PASS\n', 'ACTUAL_CAPACITY_JSON={"outcome":"passed"}\n'):
            with self.assertRaises(RuntimeError):
                actual.parse_go_receipt(output, "ACTUAL_CAPACITY_JSON=")

    def test_shared_endpoint_retains_largest_work_using_exact_go_selector(self):
        source = Path(actual.__file__).with_name("runtime_actual_linux_test.go").read_text()
        start = source.index("\n", source.index("// actualSelectionStart:")) + 1
        end = source.index("// actualSelectionEnd", start)
        selector = source[start:end]
        # Actual pinned ordering has Geo's largest table after its first table;
        # ROR's first table is its largest. Exercise the real add/largest seam,
        # including identical provider endpoints and the full selected body.
        regression = r'''
func TestSharedEndpointLargestWork(t *testing.T) {
 selection := actualWorkSelector{}
 fixtures := []struct { provider, table string; rows int64 }{
  {"geonames", "geonames_admin1", 3865},
  {"geonames", "geonames_alternate_names", 844831},
  {"ror", "locations", 141722},
  {"ror", "organizations", 141528},
  {"ror", "relationships", 72747},
 }
 for _, fixture := range fixtures {
  name := fixture.provider+"/"+fixture.table
  selection.add(fixture.provider, fixture.rows, actualWork{name:name, path:"/v1/databases/"+fixture.provider+"/dtql", body:name+"-body", want:200})
 }
 if len(selection.works)!=len(fixtures) { t.Fatalf("shared endpoints dropped selected works: %d",len(selection.works)) }
 largest:=selection.largest()
 if len(largest)!=2 || largest[0].name!="geonames/geonames_alternate_names" || largest[1].name!="ror/locations" { t.Fatalf("wrong largest works: %+v",largest) }
 if largest[0].body!="geonames/geonames_alternate_names-body" || largest[1].body!="ror/locations-body" { t.Fatal("largest request body lost") }
}
'''
        with tempfile.TemporaryDirectory() as temporary:
            probe = Path(temporary)/"selection_test.go"
            probe.write_text('package main\nimport "testing"\n'+selector+regression)
            result = subprocess.run(["go", "test", str(probe), "-run", "^TestSharedEndpointLargestWork$", "-count=1", "-v"], capture_output=True, text=True, timeout=60)
            self.assertEqual(0, result.returncode, result.stdout+result.stderr)
            self.assertIn("--- PASS: TestSharedEndpointLargestWork", result.stdout)
            self.assertNotIn("--- SKIP:", result.stdout)

    def test_exact_workflow_head_refuses_merge_head_before_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary)/"report.json"
            with patch.dict(actual.os.environ, {"GITHUB_ACTIONS":"true", "OVDB_ACTUAL_EXPECTED_SOURCE":"b"*40}), patch.object(actual.sys, "argv", ["test", "--report", str(report)]), patch.object(actual.Commands, "run", return_value="a"*40), patch.object(actual, "experiment") as experiment, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(1, actual.main())
                experiment.assert_not_called()
            receipt = json.loads(report.read_bytes())
            self.assertEqual("a"*40, receipt["source_head"])
            self.assertEqual("b"*40, receipt["expected_source_head"])
            self.assertEqual("failed", receipt["outcome"])
            self.assertIn("exact workflow source head", receipt["primary_error"])
            self.assertTrue(receipt["cleanup_complete"])

    def test_hosted_source_binding_rejects_missing_or_noncanonical_revision(self):
        with patch.dict(actual.os.environ, {"GITHUB_ACTIONS":"true"}):
            for expected in ("", "main", "A"*40):
                with self.assertRaises(ValueError):
                    actual.validate_source_head("a"*40, expected)
            actual.validate_source_head("a"*40, "a"*40)

    def test_native_proof_covers_diagnostic_values_not_just_selected_helpers(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            before = root/"before.sqlite"
            with sqlite3.connect(before) as connection:
                connection.executescript("CREATE TABLE selected(id TEXT PRIMARY KEY, value TEXT); INSERT INTO selected VALUES('key','original'); CREATE TABLE diagnostic(id TEXT, value TEXT); INSERT INTO diagnostic VALUES('hidden','original');")
            prepare_fixture.prepare(before, root/"output", "candidate", prepare_fixture.sha256_file(before), "natural", serving_adapter="separate-id/1", selected_tables=["selected"])
            after = root/"output"/"candidate.sqlite"
            proof = actual.native_proof(before, after, ["selected"])
            self.assertEqual(2, len(proof))
            self.assertEqual(1, sum(table.get("keys_scanned", 0) for table in proof))
            with sqlite3.connect(after) as connection:
                connection.execute("UPDATE diagnostic SET value='changed'")
            with self.assertRaisesRegex(ValueError, "typed native values changed"):
                actual.native_proof(before, after, ["selected"])


if __name__ == "__main__":
    unittest.main()
