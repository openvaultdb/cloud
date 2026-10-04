import json
import tempfile
import unittest
from pathlib import Path

import prepare_providers


PROJECTS_ROOT = Path(__file__).resolve().parents[6]
INVENTORY = Path(__file__).with_name("providers.json")


class PrepareProvidersTest(unittest.TestCase):
    def test_pinned_provider_repositories_prepare_a_runtime_inventory(self) -> None:
        providers_root = PROJECTS_ROOT
        providers = prepare_providers.load_inventory(INVENTORY)
        self.assertEqual(["chinook", "northwind", "pubs"], [provider["id"] for provider in providers])
        self.assertFalse(providers[2]["requirePublishedQuery"])
        if not all((providers_root / provider["repository"]).is_dir() for provider in providers):
            self.skipTest("local provider clones are absent; CI exercises immutable remote fetches during fixture preparation")
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            runtime_path = prepare_providers.prepare_inventory(INVENTORY, output, providers_root)
            runtime = json.loads(runtime_path.read_text())
            self.assertEqual(1, runtime["version"])
            self.assertEqual(["chinook", "northwind", "pubs"], [database["id"] for database in runtime["databases"]])
            for database in runtime["databases"]:
                self.assertTrue((output / database["manifest"]).is_file())
                self.assertTrue((output / database["license"]).is_file())
                self.assertEqual(40, len(database["providerRevision"]))
                self.assertIn("/", database["providerRepository"])
                self.assertTrue((output / f"{database['id']}.source-sha256").is_file())

    def test_inventory_rejects_mutable_revisions_and_unsafe_artifact_paths(self) -> None:
        document = json.loads(INVENTORY.read_text())
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "providers.json"
            document["databases"][0]["revision"] = "main"
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "immutable full Git commit SHA"):
                prepare_providers.load_inventory(path)

            document = json.loads(INVENTORY.read_text())
            document["databases"][0]["files"]["artifact"]["path"] = "../../secret.sqlite"
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "must not escape"):
                prepare_providers.load_inventory(path)

            document = json.loads(INVENTORY.read_text())
            document["databases"][0]["corsOrigins"][0] = "https://user@example.test"
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "HTTPS origins"):
                prepare_providers.load_inventory(path)

    def test_rejects_provider_bytes_that_disagree_with_pins(self) -> None:
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            prepare_providers.verify_blob(b"changed", "0" * 64, 7, "fixture")


if __name__ == "__main__":
    unittest.main()
