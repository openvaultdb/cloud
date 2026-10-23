import contextlib
import copy
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock
from urllib.error import HTTPError

import smoke_inventory as smoke


IDS = ["adventureworks", "employees", "chinook", "northwind", "pubs", "sakila", "shipping", "shipping-logs", "countries"]


def catalogue(ids=IDS):
    return {"name": "DemoDB OpenVaultDB server", "protocol": "openvaultdb/0.1", "authEnabled": False,
        "databases": [{"id": key, "url": f"https://demodb.dev/{key}/",
            "apiUrl": f"https://demodb.dev/ovdb/v1/databases/{key}",
            "manifestUrl": f"https://demodb.dev/ovdb/db/{key}/ovdb-database.json",
            "capabilities": {"read": True, "query": True, "write": False}} for key in ids]}


class PublicationDiscoveryTest(unittest.TestCase):
    def test_actual_generated_alias_shape_accepts_without_cloud_hostname(self):
        value = catalogue()
        raw = json.dumps(value).encode()
        self.assertNotIn(b"cloud.openvaultdb.com", raw)
        smoke.require_publication_discovery(raw, IDS)
        value["databases"].reverse()
        smoke.require_publication_discovery(json.dumps(value), IDS)

    def test_live_catalogue_without_countries_accepts(self):
        # demodb.dev lists only datasets with a demodb.dev host; Countries has none (demo-db/websites).
        live = catalogue([key for key in IDS if key != "countries"])
        smoke.require_publication_discovery(json.dumps(live), IDS)
        self.assertEqual(smoke.CATALOGUE_PROVIDERS, smoke.PUBLISHED_PROVIDERS - {"countries"})
        with self.assertRaises(RuntimeError):
            smoke.require_publication_discovery(json.dumps(live), [key for key in IDS if key != "countries"])

    def test_incomplete_duplicate_or_extra_inventory_refused(self):
        for mode in ["missing-catalogue-provider", "duplicate", "extra", "extra-tenth", "missing-field", "extra-field"]:
            value = catalogue()
            if mode == "missing-catalogue-provider":value["databases"] = [row for row in value["databases"] if row["id"] != "chinook"]
            if mode == "extra-tenth":value["databases"].append(dict(copy.deepcopy(value["databases"][0]), id="unpublished"))
            if mode == "duplicate":value["databases"][-1] = copy.deepcopy(value["databases"][0])
            if mode == "extra":value["databases"][-1]["id"] = "unpublished"
            if mode == "missing-field":value["databases"][0].pop("manifestUrl")
            if mode == "extra-field":value["databases"][0]["run"] = True
            with self.subTest(mode=mode), self.assertRaises(RuntimeError):
                smoke.require_publication_discovery(json.dumps(value), IDS)
        with self.assertRaises(RuntimeError):
            smoke.require_publication_discovery(json.dumps(catalogue()), IDS+[IDS[0]])

    def test_canonical_urls_cannot_hide_wrong_host_path_or_credentials(self):
        for field in ["url", "apiUrl", "manifestUrl"]:
            for replacement in ["https://cloud.openvaultdb.com/", "https://demodb.dev/", "http://demodb.dev/",
                                "https://demodb.dev@attacker.invalid/", "https://demodb.dev/path?redirect=cloud.openvaultdb.com"]:
                value=catalogue();value["databases"][0][field]=replacement
                with self.subTest(field=field,url=replacement), self.assertRaises(RuntimeError):
                    smoke.require_publication_discovery(json.dumps(value), IDS)

    def test_readonly_capability_booleans_and_protocol_are_exact(self):
        for field, values in [("read",[False,1,"true"]),("query",[False,1,"true"]),("write",[True,0,"false"])]:
            for invalid in values:
                value=catalogue();value["databases"][0]["capabilities"][field]=invalid
                with self.subTest(field=field,value=invalid),self.assertRaises(RuntimeError):
                    smoke.require_publication_discovery(json.dumps(value), IDS)
        for field, invalid in [("protocol","other"),("authEnabled",True),("authEnabled",0)]:
            value=catalogue();value[field]=invalid
            with self.assertRaises(RuntimeError):smoke.require_publication_discovery(json.dumps(value),IDS)


    def test_rollback_acceptance_records_absent_and_repinned_providers(self):
        pins = {"providerRevision": "a"*40, "sourceSha256": "b"*64, "servingSha256": "c"*64, "manifestSha256": "d"*64}
        databases = [dict(pins, id="countries", corsOrigins=["https://datatug.app"]),
                     dict(pins, id="shipping", corsOrigins=["https://datatug.app"])]
        old_pins = {"OVDB-Provider-Revision": "e"*40, "OVDB-Source-SHA256": "b"*64, "OVDB-Serving-SHA256": "c"*64, "OVDB-Manifest-SHA256": "d"*64}
        seen = []

        class Response(io.BytesIO):
            status, headers = 200, old_pins
            def __enter__(self): return self
            def __exit__(self, *args): self.close()

        def transport(request, timeout):
            seen.append(request.full_url)
            if request.full_url.endswith("/countries"):
                raise HTTPError(request.full_url, 404, "Not Found", {}, io.BytesIO(b"404 page not found"))
            return Response(b"{}")

        with tempfile.TemporaryDirectory() as directory:
            inventory = Path(directory, "inventory.json")
            inventory.write_text(json.dumps({"version": 2, "databases": databases}))
            argv = ["smoke_inventory.py", "--origin", "https://cloud.openvaultdb.com", "--inventory", str(inventory), "--rollback-acceptance"]
            with mock.patch.object(sys, "argv", argv), mock.patch.object(smoke, "urlopen", transport):
                with self.assertRaisesRegex(RuntimeError, "serves no provider"):
                    smoke.main()
            self.assertEqual(seen, ["https://cloud.openvaultdb.com/v1/databases/countries", "https://cloud.openvaultdb.com/v1/databases/shipping"])
            with mock.patch.object(sys, "argv", argv + ["--publication-receipt", str(inventory)]), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    smoke.main()

    def test_recovery_examples_never_appear_in_baseline_views(self):
        clean = {"baseline": {"attempt-11062-1-e1"}, "carrierGateway": {"attempt-11038-1-c1"},
                 "recovery11045example": {"attempt-11045-2-e1"}, "carrierGatewayRecovery11045example": {"attempt-11045-2-c1"}}
        smoke.require_recovery_separation(clean)
        self.assertEqual(set(smoke.SHIPPING_LOGS_REPRESENTATIVE_EVENTS), {"baseline", "recovery11045example", "recovery11062example", "carrierGateway", "carrierGatewayRecovery11045example", "carrierGatewayRecovery11062example"})
        for baseline, leaked in [("baseline", "attempt-11045-2-e1"), ("carrierGateway", "attempt-11045-2-c1")]:
            value = {name: set(ids) for name, ids in clean.items()}
            value[baseline].add(leaked)
            with self.subTest(baseline=baseline), self.assertRaises(RuntimeError):
                smoke.require_recovery_separation(value)


if __name__ == "__main__": unittest.main()
