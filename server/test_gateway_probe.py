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
            resolve.assert_called_once_with("google-target.example",443,type=probe.socket.SOCK_STREAM)
            self.assertFalse(Path("receipt.json").exists())

if __name__=="__main__":unittest.main()
