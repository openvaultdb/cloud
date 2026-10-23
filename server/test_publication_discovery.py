import copy
import json
import unittest

import smoke_inventory as smoke


IDS = ["adventureworks", "employees", "chinook", "northwind", "pubs", "sakila", "shipping", "shipping-logs", "countries"]


def catalogue():
    return {"name": "DemoDB OpenVaultDB server", "protocol": "openvaultdb/0.1", "authEnabled": False,
        "databases": [{"id": key, "url": f"https://demodb.dev/{key}/",
            "apiUrl": f"https://demodb.dev/ovdb/v1/databases/{key}",
            "manifestUrl": f"https://demodb.dev/ovdb/db/{key}/ovdb-database.json",
            "capabilities": {"read": True, "query": True, "write": False}} for key in IDS]}


class PublicationDiscoveryTest(unittest.TestCase):
    def test_actual_generated_alias_shape_accepts_without_cloud_hostname(self):
        value = catalogue()
        raw = json.dumps(value).encode()
        self.assertNotIn(b"cloud.openvaultdb.com", raw)
        smoke.require_publication_discovery(raw, IDS)
        value["databases"].reverse()
        smoke.require_publication_discovery(json.dumps(value), IDS)

    def test_incomplete_duplicate_or_extra_inventory_refused(self):
        for mode in ["eight", "duplicate", "extra", "missing-field", "extra-field"]:
            value = catalogue()
            if mode == "eight":value["databases"] = value["databases"][:8]
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
