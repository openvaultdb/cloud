import contextlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import gateway_probe as probe

class ProbeTest(unittest.TestCase):
    def test_protect_stage_does_not_require_not_yet_issued_mapped_certificate(self):
        with tempfile.TemporaryDirectory() as tmp, contextlib.chdir(tmp), patch("sys.argv",["probe","--phase","protect-origin","--default-url","https://gateway.run.app","--receipt","receipt.json"]), patch.object(probe,"direct",return_value={"host":"gateway.run.app","status":403}) as direct, patch.object(probe.socket,"getaddrinfo",side_effect=AssertionError("mapped DNS before issuance")):
            probe.main()
            self.assertEqual(direct.call_count,1)
            self.assertFalse(json.loads(Path("receipt.json").read_text())["automaticRenewalObserved"])
    def test_no_observation_or_direct_success_stays_red(self):
        for supplied,status in ((False,403),(True,200)):
            args=["probe","--phase","protect-origin","--receipt","receipt.json"]
            if supplied: args += ["--default-url","https://gateway.run.app"]
            with tempfile.TemporaryDirectory() as tmp, contextlib.chdir(tmp), patch("sys.argv",args), patch.object(probe,"direct",return_value={"host":"gateway.run.app","status":status}):
                with self.assertRaises(RuntimeError): probe.main()
                self.assertFalse(Path("receipt.json").exists())
    def test_mapped_origin_uses_google_target_and_branded_sni_not_edge_dns(self):
        def direct(host,address,path):
            self.assertEqual(address,"192.0.2.9")
            self.assertIn(host,("cloud.openvaultdb.com","ovdb.demodb.dev"))
            return {"host":host,"status":200}
        with tempfile.TemporaryDirectory() as tmp, contextlib.chdir(tmp), patch("sys.argv",["probe","--phase","mapped","--receipt","receipt.json"]), patch.dict(probe.os.environ,{"OVDB_GOOGLE_ORIGIN_TARGET":"google-target.example"}), patch.object(probe.socket,"getaddrinfo",return_value=[(None,None,None,None,("192.0.2.9",443))]) as resolve, patch.object(probe,"direct",side_effect=direct):
            with self.assertRaisesRegex(RuntimeError,"mapped origin bypass"): probe.main()
            resolve.assert_called_once_with("google-target.example",443,family=probe.socket.AF_INET,type=probe.socket.SOCK_STREAM)
            self.assertFalse(Path("receipt.json").exists())

class DirectCredentialVariantTest(unittest.TestCase):
    def test_duplicate_invalid_values_are_sent_as_two_headers_without_real_secret(self):
        from unittest.mock import Mock
        tls=Mock()
        tls.getpeercert.side_effect=lambda binary_form=False: b"certificate" if binary_form else {"notAfter":"future"}
        connection=Mock()
        connection.getresponse.return_value.status=403
        context=Mock();context.wrap_socket.return_value=tls
        with patch.object(probe.http.client,"HTTPSConnection",return_value=connection),patch.object(probe.socket,"create_connection"),patch.object(probe.ssl,"create_default_context",return_value=context):
            result=probe.direct("service.run.app","service.run.app","/v1",credential_variant="duplicate")
        credential_headers=[call for call in connection.putheader.call_args_list if call.args[0]=="X-OVDB-Origin-Secret"]
        self.assertEqual(len(credential_headers),2)
        self.assertTrue(all(call.args[1]=="invalid-bootstrap-proof" for call in credential_headers))
        self.assertEqual(result["credentialVariant"],"duplicate")
        self.assertEqual(result["status"],403)

if __name__=="__main__":unittest.main()
