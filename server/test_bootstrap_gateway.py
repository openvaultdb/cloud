import contextlib
import copy
import io
import json
import os
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

import bootstrap_gateway as bootstrap
import deploy_gateway as deploy

SHA="a"*40
IMAGE="europe-west1-docker.pkg.dev/openvaultdb/chinook-ovdb/server@sha256:"+"a"*64

class BootstrapTest(unittest.TestCase):
    def setUp(self):
        self.directory=tempfile.TemporaryDirectory()
        self.cwd=contextlib.chdir(self.directory.name)
        self.cwd.__enter__()
        self.environment=patch.dict(os.environ,{"PROJECT_ID":"openvaultdb","REGION":"europe-west1","SERVICE_NAME":"chinook-ovdb","DEPLOY_SHA":SHA,"BOOTSTRAP_IMAGE_DIGEST":IMAGE,"SNEAT_API_ORIGIN":"https://existing-sneat.a.run.app","OVDB_ORIGIN_SECRET_BINDING":"origin:1","OVDB_DEVICE_AUTH_PROXY_SECRET_BINDING":"proxy:2","OVDB_DEFAULT_ORIGIN_URL":"https://service.run.app"})
        self.environment.start()
        self.service={"apiVersion":"serving.knative.dev/v1","kind":"Service","metadata":{"uid":"uid1","name":"chinook-ovdb","namespace":"123456","generation":1,"resourceVersion":"rv1","annotations":{bootstrap.DISABLED:"false","unrelated":"preserve"}},"spec":{"traffic":[{"revisionName":"naked54","percent":100,"tag":"old"}],"template":{"metadata":{"name":"naked54","annotations":{"autoscaling.knative.dev/maxScale":"2"},"labels":{"source-sha":"b"*40}},"spec":{"serviceAccountName":"chinook-runtime@openvaultdb.iam.gserviceaccount.com","containerConcurrency":2,"timeoutSeconds":120,"containers":[{"image":"old-image","ports":[{"containerPort":8080}],"resources":{"limits":{"memory":"512Mi","cpu":"1"}},"env":[{"name":"UNRELATED","value":"preserve"}]}]}}},"status":{"url":"https://service.run.app","address":{"url":"https://alias.run.app"},"observedGeneration":1,"conditions":[{"type":"Ready","status":"True"}],"traffic":[{"revisionName":"naked54","percent":100,"tag":"old","url":"https://old-tag.run.app"}]}}
        self.mapping=[]
        self.calls=[]
        self.revisions={}
        self.conflict=False
        self.variant_failure=None
        self.probe_failure=None
        self.cloud=patch.object(deploy,"cloud",side_effect=self.read_cloud)
        self.cas=patch.object(bootstrap,"replace_service",side_effect=self.replace)
        self.probe=patch.object(bootstrap,"direct",side_effect=self.direct)
        self.fresh=patch.object(deploy,"require_current_main")
        for item in (self.cloud,self.cas,self.probe,self.fresh): item.start()
        self.addCleanup(self.cleanup)

    def cleanup(self):
        for item in (self.cloud,self.cas,self.probe,self.fresh,self.environment): item.stop()
        self.cwd.__exit__(None,None,None)
        self.directory.cleanup()

    def read_cloud(self,*args):
        if args[:3]==("run","services","describe"): return copy.deepcopy(self.service)
        if args[:3]==("run","domain-mappings","list"):
            if isinstance(self.mapping,Exception): raise self.mapping
            return copy.deepcopy(self.mapping)
        if args[:3]==("run","revisions","describe"): return copy.deepcopy(self.revisions[args[3]])
        raise AssertionError(args)

    def replace(self,before,desired):
        self.calls.append(copy.deepcopy(desired))
        if self.conflict: raise bootstrap.Conflict("synthetic HTTP409")
        self.assertEqual(before["metadata"]["resourceVersion"],self.service["metadata"]["resourceVersion"])
        self.assertEqual(desired["metadata"]["uid"],"uid1")
        self.assertEqual(desired["metadata"]["annotations"]["unrelated"],"preserve")
        self.assertEqual(desired["spec"]["template"]["spec"]["serviceAccountName"],"chinook-runtime@openvaultdb.iam.gserviceaccount.com")
        self.service=copy.deepcopy(desired)
        generation=before["metadata"]["generation"]+1
        self.service["metadata"].update(generation=generation,resourceVersion="rv"+str(generation))
        traffic=copy.deepcopy(desired["spec"]["traffic"])
        self.service["status"]={"observedGeneration":generation,"conditions":[{"type":"Ready","status":"True"}],"traffic":traffic}
        if not bootstrap.disabled(desired): self.service["status"]["url"]="https://service.run.app"
        template=desired["spec"]["template"]
        name=template["metadata"]["name"]
        self.revisions[name]={"metadata":template["metadata"],"spec":template["spec"],"status":{"imageDigest":template["spec"]["containers"][0]["image"],"conditions":[{"type":"Ready","status":"True"}]}}
        return copy.deepcopy(self.service)

    def direct(self,host,address,path,credential_variant="missing"):
        if self.probe_failure: raise self.probe_failure
        status=404 if (bootstrap.disabled(self.service) and host.endswith(".run.app")) or host=="old-tag.run.app" else 403
        if credential_variant==self.variant_failure: status=200
        return {"host":host,"path":path,"status":status,"certificateSHA256":"f"*64,"credentialVariant":credential_variant}

    def inventory(self,**changes):
        value={"contract":"ovdb-bootstrap-ingress-inventory/1","coverage":["project-region-mappings","service-direct-alias-tags","bounded-service-public-ingress"],"target":bootstrap.target(),"fence":bootstrap.fence(self.service),"complete":True,"mappingResources":[],"otherPublicIngress":[],"directGoogleURLs":["https://service.run.app","https://alias.run.app","https://old-tag.run.app"],"provenance":[{"source":"root complete bounded inventory","resultSHA256":"f"*64,"complete":True}],"observedAt":int(time.time())}
        value.update(changes)
        Path("ingress.json").write_text(json.dumps(value))
        return "ingress.json"

    def controller(self): return bootstrap.Bootstrap("ovdb-bootstrap-state.json")
    def capture(self): self.controller().run("capture",self.inventory())
    def quiesce(self): self.capture();self.controller().run("quiesce")
    def protect(self): self.quiesce();self.controller().run("protect")

    def test_quiescence_precedes_protected_deploy_and_open_is_gated(self):
        self.protect()
        state=self.controller().state
        self.assertEqual(state["stage"],"protected-serving-default-disabled")
        self.assertFalse(state["routesAccepted"])
        self.assertFalse(state["functionalRollbackAccepted"])
        self.assertEqual(state["directGoogleURLs"],["https://alias.run.app","https://old-tag.run.app","https://service.run.app"])
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls))
        self.assertTrue(all(not any(row.get("tag") for row in call["spec"]["traffic"]) for call in self.calls))
        self.assertEqual(self.calls[0]["spec"]["template"]["metadata"]["name"],"naked54")
        self.assertEqual(self.calls[1]["spec"]["traffic"],[{"revisionName":"naked54","percent":100}])
        self.controller().run("open-for-mapping")
        self.assertFalse(bootstrap.disabled(self.service))
        self.controller().run("status")
        state=self.controller().state
        self.assertIn("actual both-host data/auth",state["pendingExternal"])
        self.assertFalse(state["routesAccepted"])
        self.assertFalse(state["functionalRollbackAccepted"])

    def test_inconclusive_mapping_or_ingress_inventory_never_mutates(self):
        for value in (RuntimeError("permission error"),{},[{"metadata":{"name":bootstrap.HOSTS[0]}}],[{"metadata":{"name":"other.example"},"spec":{"routeName":"chinook-ovdb"}}]):
            with self.subTest(value=str(value)):
                self.mapping=value
                with self.assertRaises(RuntimeError): self.controller().run("capture",self.inventory())
                self.assertEqual(self.calls,[])
        self.mapping=[]
        for changes in ({"complete":False},{"otherPublicIngress":["LB"]},{"observedAt":0},{"provenance":[]}):
            with self.assertRaises(RuntimeError): self.controller().run("capture",self.inventory(**changes))
        self.assertEqual(self.calls,[])

    def test_uid_or_generation_drift_stops_without_replacement_by_name(self):
        self.capture()
        self.service["metadata"]["uid"]="replacement"
        with self.assertRaises(bootstrap.Conflict): self.controller().run("quiesce")
        self.assertEqual(self.calls,[])
        self.assertIn("unresolved",self.controller().state)

    def test_cas409_does_not_retry_or_restore_naked_state(self):
        self.capture();self.conflict=True
        with self.assertRaises(bootstrap.Conflict): self.controller().run("quiesce")
        self.assertEqual(len(self.calls),1)
        self.assertTrue(bootstrap.disabled(self.calls[0]))
        self.assertIn("unresolved",self.controller().state)
        with self.assertRaises(bootstrap.Conflict): self.controller().run("quiesce")
        self.assertEqual(len(self.calls),1)

    def test_premature_open_never_enables_default(self):
        self.quiesce()
        with self.assertRaises(RuntimeError): self.controller().run("open-for-mapping")
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls))

    def test_variant_bypass_after_open_recloses_and_never_restores_naked(self):
        self.protect();self.variant_failure="duplicate"
        with self.assertRaises(RuntimeError): self.controller().run("open-for-mapping")
        self.assertTrue(bootstrap.disabled(self.service))
        self.assertFalse(bootstrap.tags(self.service))
        self.assertEqual(self.controller().state["stage"],"quiesced-after-failure")
        candidate=self.controller().state["candidate"]["revision"]
        self.assertEqual(deploy.traffic(self.service),[{"revisionName":candidate,"percent":100}])
        self.assertFalse(any(call["spec"]["traffic"]==[{"revisionName":"naked54","percent":100}] and not bootstrap.disabled(call) for call in self.calls))

    def test_network_failure_is_not_rejection_and_stays_unresolved(self):
        self.capture();self.probe_failure=OSError("TLS or DNS failed")
        with self.assertRaises(RuntimeError): self.controller().run("quiesce")
        self.assertIn("unresolved",self.controller().state)
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls))

    def test_resume_rereads_actual_state_not_receipt_label(self):
        self.protect()
        self.service["spec"]["traffic"]=[{"revisionName":"naked54","percent":100}]
        with self.assertRaises(bootstrap.Conflict): self.controller().run("open-for-mapping")
        self.assertTrue(bootstrap.disabled(self.service))

    def test_protection_only_cannot_seal_as_functional_acceptance(self):
        self.protect();self.controller().run("open-for-mapping")
        Path("acceptance.json").write_text(json.dumps(self.controller().state))
        with self.assertRaises(RuntimeError): self.controller().run("seal-after-acceptance",acceptance_path="acceptance.json")
        self.assertFalse(self.controller().state["routesAccepted"])
        self.assertFalse(self.controller().state["functionalRollbackAccepted"])
        self.assertTrue(bootstrap.disabled(self.service))

    def test_generation_drift_and_5xx_probe_cannot_pass_closure(self):
        self.capture()
        self.service["metadata"]["generation"]+=1
        with self.assertRaises(bootstrap.Conflict): self.controller().run("quiesce")
        self.assertEqual(self.calls,[])

    def test_startup_identity_failure_holds_closed_no_naked_restore(self):
        self.quiesce()
        with patch.object(deploy,"secure_revision",side_effect=RuntimeError("startup identity failed")):
            with self.assertRaises(RuntimeError): self.controller().run("protect")
        self.assertTrue(bootstrap.disabled(self.service))
        self.assertFalse(bootstrap.tags(self.service))
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls))
        self.assertFalse(self.controller().state["functionalRollbackAccepted"])

    def test_reconciliation_lag_is_not_success(self):
        self.capture()
        real_read=self.read_cloud
        def lag(*args):
            value=real_read(*args)
            if args[:3]==("run","services","describe") and self.calls:
                value["status"]["observedGeneration"]=1
            return value
        with patch.object(deploy,"cloud",side_effect=lag),patch.object(bootstrap.time,"sleep"):
            with self.assertRaises(RuntimeError): self.controller().run("quiesce")
        self.assertNotEqual(self.controller().state["stage"],"quiesced")
        self.assertNotIn("unresolved",self.controller().state)
        self.assertIn("pending",self.controller().state)

    def mapped_acceptance_fixture(self):
        self.protect();self.controller().run("open-for-mapping")
        self.mapping=[{"metadata":{"name":host,"uid":"mapping-"+host,"generation":1},"spec":{"routeName":"chinook-ovdb"},"status":{"observedGeneration":1,"resourceRecords":[{"type":"A","rrdata":"192.0.2.99"}],"conditions":[{"type":"Ready","status":"True"}]}} for host in bootstrap.HOSTS]
        proof={"contract":"ovdb-functional-mapped-acceptance/1","serviceUid":"uid1","identity":self.controller().state["candidate"],"publicHosts":bootstrap.HOSTS,"originRejected":True,"routesAccepted":True,"authenticatedJourneyAccepted":True,"completeIngressInventory":True,"provenance":["root actual independent journey"],"observedAt":int(time.time()),"originIdentity":{**bootstrap.target(),"defaultUrl":"https://service.run.app"},"evidenceSHA256":{key:"f"*64 for key in ("dns","originCertificates","edgeRules","dataPinsStreaming","authenticatedDeviceDatabase")}}
        Path("acceptance.json").write_text(json.dumps(proof))
        Path("fixture/output").mkdir(parents=True)
        Path("fixture/output/inventory.json").write_text(json.dumps({"databases":[{"id":name} for name in ("adventureworks","employees","chinook","northwind","pubs","sakila","shipping","shipping-logs","countries")]}))
    def test_real_external_gates_precede_final_default_retirement(self):
        self.mapped_acceptance_fixture()
        with patch.object(bootstrap.subprocess,"run") as run:
            self.controller().run("seal-after-acceptance",acceptance_path="acceptance.json")
        self.assertEqual(run.call_count,2)
        self.assertTrue(bootstrap.disabled(self.service))
        self.assertEqual(self.controller().state["stage"],"mapped-accepted-default-disabled")
        self.assertTrue(self.controller().state["functionalRollbackAccepted"])
        self.assertFalse(self.controller().state["routesAccepted"])
        self.assertNotIn("identity",self.controller().state)

    def test_acknowledged_unknown_startup_resumes_same_generation_when_ready(self):
        self.quiesce()
        real_read=self.read_cloud
        def delayed(*args):
            value=real_read(*args)
            if args[:3]==("run","services","describe") and len(self.calls)>=2:
                value["status"]["observedGeneration"]=value["metadata"]["generation"]-1
                value["status"]["conditions"]=[{"type":"Ready","status":"Unknown"}]
            return value
        with patch.object(deploy,"cloud",side_effect=delayed),patch.object(bootstrap.time,"sleep"):
            with self.assertRaises(RuntimeError): self.controller().run("protect")
            writes=len(self.calls)
            with self.assertRaises(RuntimeError): self.controller().run("protect")
            self.assertEqual(len(self.calls),writes)
        state=self.controller().state
        self.assertEqual(state["stage"],"protected-deploying")
        self.assertIn("responseGeneration",state["pending"])
        self.assertNotIn("unresolved",state)
        self.assertFalse(state["functionalRollbackAccepted"])
        self.assertTrue(bootstrap.disabled(self.service))
        self.controller().run("protect")
        self.assertEqual(self.controller().state["stage"],"protected-serving-default-disabled")
        self.assertNotIn("pending",self.controller().state)
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls))

    def assert_failed_startup_not_resumable(self, condition, drift=False):
        self.quiesce()
        real_read=self.read_cloud
        def failed(*args):
            value=real_read(*args)
            if args[:3]==("run","services","describe") and len(self.calls)>=2:
                value["status"]["conditions"]=[{"type":"Ready","status":condition}]
                if drift: value["spec"]["template"]["spec"]["timeoutSeconds"]=121
            return value
        with patch.object(deploy,"cloud",side_effect=failed),patch.object(bootstrap.time,"sleep"):
            with self.assertRaises(RuntimeError): self.controller().run("protect")
        self.assertIn("unresolved",self.controller().state)
        self.assertFalse(self.controller().state["functionalRollbackAccepted"])
        self.assertTrue(bootstrap.disabled(self.service))

    def test_actual_failed_startup_is_not_pending_success(self):
        self.assert_failed_startup_not_resumable("False")

    def test_pending_config_drift_is_not_resumable(self):
        self.assert_failed_startup_not_resumable("Unknown",drift=True)

    def test_mapped_transient_probe_retry_stays_closed_and_requires_fresh_acceptance(self):
        self.mapped_acceptance_fixture()
        with patch.object(bootstrap.subprocess,"run",side_effect=RuntimeError("transient probe failure")):
            with self.assertRaises(RuntimeError): self.controller().run("seal-after-acceptance",acceptance_path="acceptance.json")
        state=self.controller().state
        self.assertEqual(state["stage"],"protected-mapped-default-disabled")
        self.assertNotIn("unresolved",state)
        self.assertFalse(state["functionalRollbackAccepted"])
        self.assertFalse(state["routesAccepted"])
        writes=len(self.calls)
        proof=json.loads(Path("acceptance.json").read_text())
        proof["observedAt"]=int(time.time())-301
        Path("acceptance.json").write_text(json.dumps(proof))
        with patch.object(bootstrap.subprocess,"run") as run:
            with self.assertRaises(RuntimeError): self.controller().run("seal-after-acceptance",acceptance_path="acceptance.json")
            run.assert_not_called()
        self.assertEqual(self.controller().state["stage"],"protected-mapped-default-disabled")
        proof["observedAt"]=int(time.time())
        Path("acceptance.json").write_text(json.dumps(proof))
        with patch.object(bootstrap.subprocess,"run") as run:
            self.controller().run("seal-after-acceptance",acceptance_path="acceptance.json")
        self.assertEqual(run.call_count,2)
        self.assertEqual(self.controller().state["stage"],"mapped-accepted-default-disabled")
        self.assertTrue(self.controller().state["functionalRollbackAccepted"])
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls[writes:]))
        self.assertEqual(len(self.mapping),2)

    def test_missing_external_auth_or_stale_mapping_proof_recloses(self):
        self.protect();self.controller().run("open-for-mapping")
        self.mapping=[{"metadata":{"name":host,"uid":"mapping","generation":2},"spec":{"routeName":"chinook-ovdb"},"status":{"observedGeneration":1,"resourceRecords":[{}],"conditions":[{"type":"Ready","status":"True"}]}} for host in bootstrap.HOSTS]
        Path("acceptance.json").write_text("{}")
        with self.assertRaises(RuntimeError): self.controller().run("seal-after-acceptance",acceptance_path="acceptance.json")
        self.assertTrue(bootstrap.disabled(self.service))
        self.assertFalse(self.controller().state["functionalRollbackAccepted"])

    def test_5xx_is_not_closure(self):
        self.capture()
        with patch.object(bootstrap,"direct",return_value={"host":"service.run.app","path":"/v1","status":503,"certificateSHA256":"f"*64}):
            with self.assertRaises(RuntimeError): self.controller().run("quiesce")
        self.assertNotEqual(self.controller().state["stage"],"quiesced")

    def test_unrelated_path404_is_not_closure(self):
        self.capture()
        with patch.object(bootstrap,"direct",return_value={"host":"service.run.app","path":"/nonexistent","status":404,"certificateSHA256":"f"*64}):
            with self.assertRaises(RuntimeError): self.controller().run("quiesce")
        self.assertNotEqual(self.controller().state["stage"],"quiesced")

    def test_pending_bootstrap_cannot_fall_through_to_ordinary_mode(self):
        self.capture()
        before=len(self.calls)
        with patch("sys.argv",["deploy_gateway.py","--phase","mapped"]):
            with self.assertRaisesRegex(RuntimeError,"bootstrap pending"): deploy.main()
        self.assertEqual(len(self.calls),before)

    def test_surviving_zero_percent_tag_is_not_closed(self):
        self.capture()
        actual_replace=self.replace
        def retain_tag(before,desired):
            actual_replace(before,desired)
            self.service["status"]["traffic"].append({"revisionName":"naked54","percent":0,"tag":"survivor","url":"https://survivor.run.app"})
            return copy.deepcopy(self.service)
        with patch.object(bootstrap,"replace_service",side_effect=retain_tag):
            with self.assertRaises(RuntimeError): self.controller().run("quiesce")
        self.assertNotEqual(self.controller().state["stage"],"quiesced")
        self.assertIn("unresolved",self.controller().state)
        self.assertTrue(all(bootstrap.disabled(call) for call in self.calls))

    def test_lost_conditional_response_is_unresolved_without_blind_retry(self):
        self.capture()
        actual_replace=self.replace
        def crash_after_write(before,desired):
            actual_replace(before,desired)
            raise SystemExit("synthetic process exit before response persisted")
        with patch.object(bootstrap,"replace_service",side_effect=crash_after_write):
            with self.assertRaises(SystemExit): self.controller().run("quiesce")
        self.assertIn("pending",self.controller().state)
        writes=len(self.calls)
        with self.assertRaises(bootstrap.Conflict): self.controller().run("quiesce")
        self.assertEqual(len(self.calls),writes)
        self.assertIn("unresolved",self.controller().state)

    def test_persisted_conditional_response_resumes_only_its_generation(self):
        self.capture()
        read=bootstrap.Bootstrap.read
        def crash_before_poll(controller):
            if controller.state and controller.state.get("pending",{}).get("responseGeneration"):
                raise SystemExit("synthetic process exit after response persisted")
            return read(controller)
        with patch.object(bootstrap.Bootstrap,"read",crash_before_poll):
            with self.assertRaises(SystemExit): self.controller().run("quiesce")
        writes=len(self.calls)
        self.controller().run("quiesce")
        self.assertEqual(len(self.calls),writes)
        self.assertEqual(self.controller().state["stage"],"quiesced")
        self.assertNotIn("pending",self.controller().state)

class ConditionalTransportTest(unittest.TestCase):
    def setUp(self):
        self.env=patch.dict(os.environ,{"PROJECT_ID":"openvaultdb","REGION":"europe-west1","SERVICE_NAME":"chinook-ovdb"});self.env.start();self.addCleanup(self.env.stop)
        self.before={"metadata":{"uid":"same","namespace":"123456","name":"chinook-ovdb","resourceVersion":"CAPTURED-RV"},"spec":{"unrelated":"preserved"}}

    def test_original_version_and_full_same_service_body_sent_without_token_evidence(self):
        response=unittest.mock.Mock(status=200)
        response.read.return_value=json.dumps(self.before).encode()
        connection=unittest.mock.Mock();connection.getresponse.return_value=response
        auth=unittest.mock.Mock(returncode=0,stdout="EPHEMERAL-TOKEN\n")
        with patch.object(bootstrap.subprocess,"run",return_value=auth) as command,patch.object(bootstrap.http.client,"HTTPSConnection",return_value=connection) as https:
            result=bootstrap.replace_service(self.before,copy.deepcopy(self.before))
        https.assert_called_once_with("europe-west1-run.googleapis.com",timeout=30)
        args,kwargs=connection.request.call_args
        self.assertEqual(args,("PUT","/apis/serving.knative.dev/v1/namespaces/123456/services/chinook-ovdb"))
        self.assertEqual(json.loads(kwargs["body"])["metadata"]["resourceVersion"],"CAPTURED-RV")
        self.assertEqual(json.loads(kwargs["body"])["spec"],self.before["spec"])
        self.assertNotIn("EPHEMERAL-TOKEN",json.dumps(result))
        self.assertNotIn("EPHEMERAL-TOKEN",str(command.call_args))

    def test_changed_expected_uid_or_version_is_never_sent(self):
        changed=copy.deepcopy(self.before);changed["metadata"]["resourceVersion"]="NEWER-RV"
        with patch.object(bootstrap.http.client,"HTTPSConnection") as https:
            with self.assertRaises(bootstrap.Conflict): bootstrap.replace_service(self.before,changed)
        https.assert_not_called()

    def test_http409_and_other_errors_never_read_or_expose_response_body(self):
        for status in (409,403,500,302):
            response=unittest.mock.Mock(status=status)
            response.read.side_effect=AssertionError("error body must not be read")
            connection=unittest.mock.Mock();connection.getresponse.return_value=response
            with patch.object(bootstrap.subprocess,"run",return_value=unittest.mock.Mock(returncode=0,stdout="EPHEMERAL-TOKEN")),patch.object(bootstrap.http.client,"HTTPSConnection",return_value=connection):
                with self.assertRaises(RuntimeError) as error: bootstrap.replace_service(self.before,copy.deepcopy(self.before))
            self.assertNotIn("EPHEMERAL-TOKEN",str(error.exception))
            response.read.assert_not_called()
            self.assertEqual(connection.request.call_count,1)

if __name__=="__main__":unittest.main()
