"""Portable tests of final-filesystem evidence and hostile-image construction."""
import io
import tarfile
import unittest
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
