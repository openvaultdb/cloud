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
    def test_regional_mapping_inventory_uses_supported_beta_track(self):
        with patch.dict(os.environ, {"PROJECT_ID": "openvaultdb", "REGION": "europe-west1"}), patch.object(deploy.subprocess, "check_output", return_value="[]") as output:
            self.assertEqual(deploy.cloud("run", "domain-mappings", "list", "--platform=managed"), [])
            output.assert_called_once_with(["gcloud", "beta", "run", "domain-mappings", "list", "--platform=managed", "--project=openvaultdb", "--region=europe-west1", "--format=json"], text=True)

    def test_service_inventory_keeps_stable_track_and_failure_propagates(self):
        with patch.dict(os.environ, {"PROJECT_ID": "openvaultdb", "REGION": "europe-west1"}), patch.object(deploy.subprocess, "check_output", side_effect=RuntimeError("inventory unavailable")) as output:
            with self.assertRaisesRegex(RuntimeError, "inventory unavailable"):
                deploy.cloud("run", "services", "describe", "chinook-ovdb")
            output.assert_called_once_with(["gcloud", "run", "services", "describe", "chinook-ovdb", "--project=openvaultdb", "--region=europe-west1", "--format=json"], text=True)

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
                # The rollback revision predates the HTTP probe and keeps TCP.
                startup={"httpGet":{"path":"/healthz/startup","port":8080},"periodSeconds":10,"timeoutSeconds":1,"failureThreshold":24} if candidate else {"tcpSocket":{"port":8080}}
                return {"status":{"imageDigest":image},"metadata":{"labels":{"source-sha":source}},"spec":{"containers":[{"ports":[{"containerPort":8080}],"startupProbe":startup,"env":[{"name":"OVDB_ORIGIN_SECRET","valueFrom":{"secretKeyRef":{"name":"origin","key":"1"}}}]}]}}
            if args[:3]==("run","services","describe"):
                status={"traffic":[{"revisionName":state["revision"],"percent":100}]}
                if scenario not in ("no-default-url","no-default-url-live-failure"): status["url"]="https://service.run.app"
                if scenario=="wrong-serving-traffic" and state["revision"]=="created-candidate":
                    status["traffic"]=[{"revisionName":"unrelated-prior","percent":100}]
                if scenario=="rollback-verification-failure" and state["revision"]=="secure-prior":
                    status["traffic"]=[{"revisionName":"unrelated-prior","percent":100}]
                container={"ports":[{"containerPort":8080}],"startupProbe":{"httpGet":{"path":"/health"}}}
                if scenario=="http-liveness": container["livenessProbe"]={"httpGet":{"path":"/health"}}
                if scenario=="http-readiness": container["readinessProbe"]={"httpGet":{"path":"/health"}}
                return {"status":status,"spec":{"template":{"spec":{"containers":[container]}}}}
            if args[:2]==("run","deploy"):
                self.assertIn("--no-traffic",args)
                self.assertIn("--port=8080",args)
                self.assertIn("--startup-probe=httpGet.path=/healthz/startup,httpGet.port=8080,periodSeconds=10,timeoutSeconds=1,failureThreshold=24",args)
                self.assertIn("--min-instances=0",args)
                self.assertFalse(any(a.startswith(("--liveness-probe","--readiness-probe")) for a in args))
                self.assertIn("--image="+IMAGE,args)
                self.assertEqual("--no-default-url" in args,phase=="mapped")
                if scenario=="startup-failure":
                    raise RuntimeError("synthetic attempted deployment startup failure")
                return {"status":{"latestCreatedRevisionName":"created-candidate"}}
            if args[:3]==("run","services","update-traffic"):
                route=next(a for a in args if a.startswith("--to-revisions="))
                self.assertNotIn("LATEST",route)
                state["revision"]=route.split("=",1)[1].rsplit("=",1)[0]
                return {}
            raise AssertionError(args)
        git_reads=0
        def output(args,**kwargs):
            nonlocal git_reads
            if args[0] == "git":
                git_reads += 1
                stale = scenario=="stale-main" or scenario=="late-stale-main" and git_reads>1
                return ("d"*40 if stale else SHA)+"\trefs/heads/main\n"
            return "sha256:"+"a"*64
        def run(args,**kwargs):
            calls.append(tuple(args))
            if scenario in ("live-failure","no-default-url-live-failure","rollback-verification-failure") and "gateway-live-receipt.json" in args:
                raise RuntimeError("synthetic live failure")
        environment={"PROJECT_ID":"test-project","REGION":"test-region","OVDB_DEFAULT_ORIGIN_URL":"https://service.run.app","DEPLOY_SHA":SHA,"OVDB_ORIGIN_SECRET_BINDING":"origin:1","OVDB_DEVICE_AUTH_PROXY_SECRET_BINDING":"proxy:2","SNEAT_API_ORIGIN":"https://sneat.example","SERVICE_NAME":"gateway","OVDB_SECURE_ROLLBACK_REVISION":"secure-prior","IMAGE":"registry.example/server:source"}
        with tempfile.TemporaryDirectory() as tmp, contextlib.chdir(tmp), patch.dict(os.environ,environment), patch.object(deploy,"cloud",side_effect=cloud), patch.object(deploy.subprocess,"check_output",side_effect=output), patch.object(deploy.subprocess,"run",side_effect=run), patch("sys.argv",["deploy_gateway.py","--phase",phase]):
            if scenario=="invalid-retained-url": os.environ["OVDB_DEFAULT_ORIGIN_URL"]="https://attacker.example"
            if scenario=="unbound-retained-url": os.environ["OVDB_DEFAULT_ORIGIN_URL"]="https://other.run.app"
            Path("secure-rollback-acceptance.json").write_text(json.dumps({"identity":rollback,"originRejected":proof,"routesAccepted":True,"originIdentity":{"project":"test-project","region":"test-region","service":"gateway","defaultUrl":"https://service.run.app"}}))
            error=None
            try: deploy.main()
            except RuntimeError as exc: error=exc
            receipt=json.loads(Path("shipping-publication-deploy-receipt.json").read_text()) if Path("shipping-publication-deploy-receipt.json").exists() else None
            restored=json.loads(Path("shipping-publication-rollback-receipt.json").read_text()) if Path("shipping-publication-rollback-receipt.json").exists() else None
        return calls,state,error,receipt,restored
    def test_startup_probe_window_and_accepted_shapes(self):
        fields=dict(item.split("=",1) for item in deploy.STARTUP_PROBE.split(","))
        self.assertEqual(fields["httpGet.path"],"/healthz/startup")
        self.assertEqual(fields["httpGet.port"],"8080")
        # Cloud Run allows at most 240 s; the Go budget (200 s) plus one period fits.
        self.assertLessEqual(int(fields["periodSeconds"])*int(fields["failureThreshold"]),240)
        self.assertGreaterEqual(int(fields["periodSeconds"])*int(fields["failureThreshold"]),200+int(fields["periodSeconds"]))
        container=lambda probe:{"ports":[{"containerPort":8080}],"startupProbe":probe}
        for probe in ({"tcpSocket":{"port":8080}},{"httpGet":{"path":"/healthz/startup","port":8080}},{"httpGet":{"path":"/healthz/startup"}}):
            deploy.check_probes(container(probe),True)
        for probe in ({},{"tcpSocket":{"port":9090}},{"httpGet":{"path":"/health","port":8080}},{"httpGet":{"path":"/healthz/startup","port":9090}},
                      {"httpGet":{"path":"/healthz/startup","port":8080,"httpHeaders":[{"name":"X-OVDB-Origin-Secret","value":"x"}]}},
                      {"httpGet":{"path":"/healthz/startup","port":8080},"tcpSocket":{"port":8080}},{"grpc":{"port":8080}},
                      {"grpc":{"port":8080},"tcpSocket":{"port":8080}}):
            with self.assertRaisesRegex(RuntimeError,"startup probe"):
                deploy.check_probes(container(probe),True)
        with self.assertRaisesRegex(RuntimeError,"pinned startup probe"):
            deploy.check_probes({"ports":[{"containerPort":9090}],"startupProbe":{"tcpSocket":{"port":8080}}},True)
        with self.assertRaisesRegex(RuntimeError,"liveness/readiness"):
            deploy.check_probes({"livenessProbe":{"httpGet":{"path":"/healthz/startup"}}},False)

    def test_wrong_serving_traffic_is_refused_and_secure_rollback_verified(self):
        calls,state,error,receipt,rollback=self.exercise("wrong-serving-traffic")
        self.assertIsNotNone(error)
        self.assertEqual(state["revision"],"secure-prior")
        self.assertEqual(receipt["phase"],"rolling-back")
        self.assertTrue(rollback["immutableTrafficRestored"])
        self.assertTrue(rollback["liveAcceptancePassed"])
        self.assertTrue(any("--to-revisions=created-candidate=100" in call for call in calls))
        self.assertFalse(any("gateway-live-receipt.json" in call for call in calls))

    def test_rollback_verification_failure_is_visibly_false_and_stays_red(self):
        calls,_,error,receipt,rollback=self.exercise("rollback-verification-failure")
        self.assertIsNotNone(error)
        self.assertEqual(receipt["phase"],"rolling-back")
        self.assertFalse(rollback["immutableTrafficRestored"])
        self.assertFalse(rollback["liveAcceptancePassed"])
        self.assertTrue(any("--to-revisions=secure-prior=100" in call for call in calls))

    def test_attempted_startup_failure_securely_rolls_back_and_stays_red(self):
        calls,state,error,receipt,rollback=self.exercise("startup-failure")
        self.assertIsNotNone(error)
        self.assertEqual(receipt["phase"],"rolling-back")
        self.assertTrue(any(call[:2]==("run","deploy") for call in calls))
        self.assertFalse(any("--to-revisions=created-candidate=100" in call for call in calls))
        self.assertEqual(state["revision"],"secure-prior")
        self.assertTrue(rollback["immutableTrafficRestored"])
        self.assertTrue(rollback["liveAcceptancePassed"])

    def test_rollback_preserves_protected_fixture_without_scratch_writes(self):
        original_write=Path.write_text
        observed=[]
        def checked_write(path,data,*args,**kwargs):
            if path.name=="secure-rollback-acceptance.json":
                output=Path("fixture/output")
                output.mkdir(parents=True)
                inventory=output/"inventory.json"
                original_write(inventory,'{"databases":[]}')
                inventory.chmod(0o444)
                output.chmod(0o555)
                output.parent.chmod(0o555)
            protected=Path("fixture/output").resolve()
            self.assertFalse(path.resolve().is_relative_to(protected),"controller attempted a protected fixture write")
            if protected.exists():
                self.assertEqual(protected.stat().st_mode & 0o777,0o555)
                self.assertEqual(protected.parent.stat().st_mode & 0o777,0o555)
                self.assertEqual((protected/"inventory.json").stat().st_mode & 0o777,0o444)
                self.assertEqual((protected/"inventory.json").read_text(),'{"databases":[]}')
                self.assertEqual(sorted(entry.name for entry in protected.iterdir()),["inventory.json"])
                observed.append(path.name)
            return original_write(path,data,*args,**kwargs)
        with patch.object(Path,"write_text",checked_write):
            calls,_,error,receipt,rollback=self.exercise("live-failure")
        self.assertIsNotNone(error)
        self.assertEqual(receipt["phase"],"rolling-back")
        self.assertTrue(rollback["immutableTrafficRestored"])
        self.assertTrue(rollback["liveAcceptancePassed"])
        self.assertIn("shipping-publication-rollback-receipt.json",observed)
        smoke=[call for call in calls if "smoke_inventory.py" in call]
        self.assertTrue(smoke)
        for call in smoke:
            self.assertEqual(call[call.index("--inventory")+1],"fixture/output/inventory.json")

    def test_disabled_default_url_uses_retained_identity_for_deploy_and_rollback(self):
        for scenario in ("no-default-url","no-default-url-live-failure"):
            calls,_,error,_,restored=self.exercise(scenario)
            probes=[a for a in calls if "gateway_probe.py" in a]
            self.assertTrue(probes)
            for call in probes: self.assertEqual(call[call.index("--default-url")+1],"https://service.run.app")
            if scenario=="no-default-url": self.assertIsNone(error)
            else:
                self.assertIsNotNone(error)
                self.assertTrue(restored["liveAcceptancePassed"])
    def test_pre_mutation_stale_main_has_zero_traffic_or_deployment_writes(self):
        calls,state,error,_,restored=self.exercise("stale-main")
        self.assertIsNotNone(error)
        self.assertEqual(state["revision"],"prior")
        self.assertIsNone(restored)
        self.assertFalse(any(a[:2]==("run","deploy") or a[:3]==("run","services","update-traffic") for a in calls))
    def test_inherited_http_liveness_and_readiness_fail_before_mutation(self):
        for scenario in ("http-liveness","http-readiness"):
            calls,state,error,_,_=self.exercise(scenario)
            self.assertIsNotNone(error)
            self.assertEqual(state["revision"],"prior")
            self.assertFalse(any(a[:2]==("run","deploy") or a[:3]==("run","services","update-traffic") for a in calls))
    def test_retained_origin_rejects_invalid_or_unbound_endpoint_before_mutation(self):
        for scenario in ("invalid-retained-url","unbound-retained-url"):
            calls,state,error,_,_=self.exercise(scenario)
            self.assertIsNotNone(error)
            self.assertEqual(state["revision"],"prior")
            self.assertFalse(any(a[:2]==("run","deploy") or a[:3]==("run","services","update-traffic") for a in calls))
    def test_pinned_old_traffic_routes_only_verified_created_revision(self):
        calls,state,error,receipt,_=self.exercise()
        self.assertIsNone(error)
        self.assertEqual(state["revision"],"created-candidate")
        self.assertEqual(receipt["phase"],"public-data-accepted-auth-journey-pending")
        self.assertEqual(len([a for a in calls if a[:3]==("run","services","update-traffic")]),1)
    def test_identity_mismatch_never_routes_candidate_and_stays_red(self):
        for case in ("wrong-image","wrong-source","live-failure","late-stale-main"):
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
