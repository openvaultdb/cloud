"""Portable tests of final-filesystem evidence and hostile-image construction."""
import io
import json
import subprocess
import tarfile
import unittest
from unittest import mock
import tempfile
from pathlib import Path
import prepare_providers

import test_image_runtime


def exported_fixture():
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w") as archive:
        for name, mode, content in [("srv", 0o755, None), ("srv/fixture", 0o555, None),
                                    ("tmp", 0o1777, None), ("srv/fixture/inventory.json", 0o444, b"{}"),
                                    ("srv/fixture/candidate.sqlite", 0o444, b"SQLite format 3\0" + b"\0\0\1\1")]:
            entry = tarfile.TarInfo(name)
            entry.mode = mode
            entry.type = tarfile.DIRTYPE if content is None else tarfile.REGTYPE
            entry.size = 0 if content is None else len(content)
            archive.addfile(entry, None if content is None else io.BytesIO(content))
    return output.getvalue()


class ImageEvidenceHarnessTest(unittest.TestCase):
    def test_final_filesystem_policy_and_hostile_entries(self):
        data = exported_fixture()
        self.assertEqual(4, len(test_image_runtime.validate_export(data)))
        for mutation in ("ancestor", "writable", "owner", "symlink", "hardlink", "fifo"):
            with self.subTest(mutation=mutation), self.assertRaises(RuntimeError):
                test_image_runtime.validate_export(test_image_runtime.hostile_export(data, mutation))

    def test_hostile_bytes_are_concrete(self):
        for mutation in ("truncated", "wal-header", "wal-sidecar"):
            data = test_image_runtime.hostile_export(exported_fixture(), mutation)
            with self.subTest(mutation=mutation), tarfile.open(fileobj=io.BytesIO(data)) as archive:
                if mutation == "wal-sidecar":
                    self.assertIsNotNone(archive.getmember("srv/fixture/candidate.sqlite-wal"))
                else:
                    content = archive.extractfile("srv/fixture/candidate.sqlite").read()
                    if mutation == "truncated":
                        self.assertEqual(10, len(content))
                    else:
                        self.assertEqual(b"\2\2", content[18:20])


class FlatPreparationLayoutTest(unittest.TestCase):
    def test_nested_hidden_and_linked_required_artifacts_are_refused(self):
        for mutation in ("nested", "hidden", "symlink", "hardlink", "missing-inventory"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary)
                (output / "inventory.json").write_text("{}")
                (output / "data.sqlite").write_bytes(b"tiny")
                if mutation == "nested": (output / "nested").mkdir()
                elif mutation == "hidden": (output / ".required.json").write_text("{}")
                elif mutation == "symlink": (output / "alias").symlink_to("data.sqlite")
                elif mutation == "hardlink": (output / "alias").hardlink_to(output / "data.sqlite")
                else: (output / "inventory.json").unlink()
                with self.assertRaises(ValueError):
                    prepare_providers.validate_image_layout(output)

    def test_preparation_flat_result_is_accepted(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            (output / "inventory.json").write_text("{}")
            (output / "data.sqlite").write_bytes(b"tiny")
            prepare_providers.validate_image_layout(output)


class CleanupFailureHarnessTest(unittest.TestCase):
    def run_main(self, experiment, subprocess_run):
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary) / "receipt.json"
            with mock.patch.dict(test_image_runtime.os.environ, {"OVDB_IMAGE_REPORT": str(report)}), \
                    mock.patch.object(test_image_runtime, "experiment", side_effect=experiment), \
                    mock.patch.object(test_image_runtime.subprocess, "run", side_effect=subprocess_run):
                result = test_image_runtime.main()
            return result, json.loads(report.read_text())

    def test_runtime_and_cleanup_timeouts_preserve_failure_receipt_and_owned_name(self):
        calls = []

        def commands(arguments, **options):
            calls.append((arguments, options))
            if arguments[0] == "git":
                return subprocess.CompletedProcess(arguments, 0, b"exact-head\n", b"")
            raise subprocess.TimeoutExpired(arguments, options["timeout"])

        def experiment(receipt, prefix, images, containers):
            images.append(prefix + ":owned-image")
            test_image_runtime.exercise(receipt, prefix, containers, images[0], "accept")

        result, receipt = self.run_main(experiment, commands)
        self.assertEqual(1, result)
        self.assertEqual("failed", receipt["outcome"])
        self.assertIn("90 seconds", receipt["error"])
        runtime = calls[1][0]
        name = runtime[runtime.index("--name") + 1]
        self.assertTrue(name.startswith("ovdb-image-test-"))
        self.assertNotIn("--rm", runtime)
        removals = [arguments for arguments, _ in calls if arguments[:3] == ["docker", "rm", "-f"]]
        self.assertEqual([["docker", "rm", "-f", name]] * 2, removals)
        self.assertEqual([name], receipt["remaining_containers"])
        self.assertEqual([name], receipt["owned_containers"])
        self.assertFalse(receipt["cleanup_complete"])
        self.assertEqual(3, len(receipt["cleanup_errors"]))
        self.assertEqual("failed", receipt["commands"][0]["outcome"])
        self.assertTrue(all(options["timeout"] <= 90 for arguments, options in calls if arguments[0] == "docker"))

    def test_cleanup_errors_cannot_turn_passing_experiment_green(self):
        for error in (subprocess.TimeoutExpired(["docker"], 60), OSError("engine unavailable"), None):
            with self.subTest(error=error):
                def commands(arguments, **options):
                    if arguments[0] == "git":
                        return subprocess.CompletedProcess(arguments, 0, b"exact-head\n", b"")
                    if error is not None:
                        raise error
                    return subprocess.CompletedProcess(arguments, 1, b"removal refused", b"")

                def experiment(receipt, prefix, images, containers):
                    images.append(prefix + ":owned-image")
                    receipt["outcome"] = "synthetic_image_pass"

                result, receipt = self.run_main(experiment, commands)
                self.assertEqual(1, result)
                self.assertEqual("failed", receipt["outcome"])
                self.assertFalse(receipt["cleanup_complete"])
                self.assertEqual(1, len(receipt["cleanup_errors"]))

    def test_successful_runtime_explicitly_removes_owned_container(self):
        calls = []

        def commands(arguments, **options):
            calls.append(arguments)
            output = b"exact-head\n" if arguments[0] == "git" else b"--- PASS: TestProtectedImageLinuxJourney\n"
            return subprocess.CompletedProcess(arguments, 0, output, b"")

        def experiment(receipt, prefix, images, containers):
            test_image_runtime.exercise(receipt, prefix, containers, "owned-image", "accept")
            receipt["outcome"] = "synthetic_image_pass"

        result, receipt = self.run_main(experiment, commands)
        self.assertEqual(0, result)
        self.assertEqual([], receipt["remaining_containers"])
        self.assertTrue(receipt["cleanup_complete"])
        runtime = calls[1]
        self.assertEqual(["docker", "rm", "-f", runtime[runtime.index("--name") + 1]], calls[2])
        self.assertEqual("passed", receipt["commands"][0]["outcome"])

    def test_source_identity_failure_still_writes_failed_receipt(self):
        result, receipt = self.run_main(mock.Mock(), mock.Mock(side_effect=OSError("git unavailable")))
        self.assertEqual(1, result)
        self.assertEqual("failed", receipt["outcome"])
        self.assertEqual("git unavailable", receipt["error"])
