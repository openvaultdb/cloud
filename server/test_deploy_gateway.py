import contextlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import deploy_gateway as deploy

SHA="a"*40
IMAGE="registry.example/server@sha256:"+"a"*64
class GatewayDeploymentTest(unittest.TestCase):
    def exercise(self, scenario="ok", phase="mapped", proof=True):
        calls=[]
        state={"revision":"prior"}
        rollback={"revision":"secure-prior","image":"registry.example/server@sha256:"+"b"*64,"sourceSha":"b"*40}
        def cloud(*args):
            calls.append(args)
            if args[:3]==("run","revisions","describe"):
                name=args[3]
                candidate=name=="created-candidate"
                image=IMAGE if candidate else rollback["image"]
                source=SHA if candidate else rollback["sourceSha"]
                if candidate and scenario=="wrong-image": image=image[:-1]+"d"
                if candidate and scenario=="wrong-source": source="d"*40
                return {"status":{"imageDigest":image},"metadata":{"labels":{"source-sha":source}},"spec":{"containers":[{"env":[{"name":"OVDB_ORIGIN_SECRET","valueFrom":{"secretKeyRef":{"name":"origin","key":"1"}}}]}]}}
            if args[:3]==("run","services","describe"):
                return {"status":{"url":"https://service.run.app","traffic":[{"revisionName":state["revision"],"percent":100}]}}
            if args[:2]==("run","deploy"):
                self.assertIn("--no-traffic",args)
                self.assertIn("--image="+IMAGE,args)
                self.assertEqual("--no-default-url" in args,phase=="mapped")
                return {"status":{"latestCreatedRevisionName":"created-candidate"}}
            if args[:3]==("run","services","update-traffic"):
                route=next(a for a in args if a.startswith("--to-revisions="))
                self.assertNotIn("LATEST",route)
                state["revision"]=route.split("=",1)[1].rsplit("=",1)[0]
                return {}
            raise AssertionError(args)
        def output(args,**kwargs):
            if args[0] == "git":
                return ("d"*40 if scenario=="stale-main" else SHA)+"\trefs/heads/main\n"
            return "sha256:"+"a"*64
        def run(args,**kwargs):
            calls.append(tuple(args))
            if scenario=="live-failure" and "gateway-live-receipt.json" in args:
                raise RuntimeError("synthetic live failure")
        environment={"DEPLOY_SHA":SHA,"OVDB_ORIGIN_SECRET_BINDING":"origin:1","OVDB_DEVICE_AUTH_PROXY_SECRET_BINDING":"proxy:2","SNEAT_API_ORIGIN":"https://sneat.example","SERVICE_NAME":"gateway","OVDB_SECURE_ROLLBACK_REVISION":"secure-prior","IMAGE":"registry.example/server:source"}
        with tempfile.TemporaryDirectory() as tmp, contextlib.chdir(tmp), patch.dict(os.environ,environment), patch.object(deploy,"cloud",side_effect=cloud), patch.object(deploy.subprocess,"check_output",side_effect=output), patch.object(deploy.subprocess,"run",side_effect=run), patch("sys.argv",["deploy_gateway.py","--phase",phase]):
            Path("secure-rollback-acceptance.json").write_text(json.dumps({"identity":rollback,"originRejected":proof,"routesAccepted":True}))
            error=None
            try: deploy.main()
            except RuntimeError as exc: error=exc
            receipt=json.loads(Path("shipping-publication-deploy-receipt.json").read_text()) if Path("shipping-publication-deploy-receipt.json").exists() else None
            restored=json.loads(Path("shipping-publication-rollback-receipt.json").read_text()) if Path("shipping-publication-rollback-receipt.json").exists() else None
        return calls,state,error,receipt,restored
    def test_pinned_old_traffic_routes_only_verified_created_revision(self):
        calls,state,error,receipt,_=self.exercise()
        self.assertIsNone(error)
        self.assertEqual(state["revision"],"created-candidate")
        self.assertEqual(receipt["phase"],"public-data-accepted-auth-journey-pending")
        self.assertEqual(len([a for a in calls if a[:3]==("run","services","update-traffic")]),1)
    def test_identity_mismatch_never_routes_candidate_and_stays_red(self):
        for case in ("wrong-image","wrong-source","live-failure","stale-main"):
            with self.subTest(case=case):
                calls,state,error,_,restored=self.exercise(case)
                self.assertIsNotNone(error)
                self.assertEqual(state["revision"],"secure-prior")
                self.assertTrue(restored["immutableTrafficRestored"])
                if case!="live-failure": self.assertFalse(any("--to-revisions=created-candidate=100" in a for a in calls))
    def test_unaccepted_rollback_fails_before_any_mutation(self):
        calls,_,error,receipt,_=self.exercise(proof=False)
        self.assertIsNotNone(error)
        self.assertIsNone(receipt)
        self.assertFalse(any(a[:2]==("run","deploy") or a[:3]==("run","services","update-traffic") for a in calls))
    def test_protect_phase_keeps_default_url_for_boundary_proof(self):
        calls,_,error,receipt,_=self.exercise(phase="protect-origin")
        self.assertIsNone(error)
        self.assertEqual(receipt["phase"],"origin-protected-public-acceptance-pending")
        self.assertTrue(any("--default-url" in a for a in calls))

if __name__=="__main__": unittest.main()
