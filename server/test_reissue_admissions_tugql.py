import contextlib
import hashlib
import io
import tempfile
import unittest
from pathlib import Path

import reissue_admissions_tugql as tool


class ReissueAdmissionsTests(unittest.TestCase):
    def test_rewrites_only_route_fields_and_prints_digest(self):
        with tempfile.TemporaryDirectory() as d:
            public = Path(d) / "public.json"
            public.write_text('{"workerPath": "/ecb-public/v1/databases/ecb/dtql",\n "goPath":"/v1/databases/ecb/dtql","note":"dtql stays"}')
            ecb = Path(d) / "ecb.json"
            ecb.write_text('{"path":"/v1/databases/ecb/dtql"}')
            iana = Path(d) / "iana.json"
            iana.write_text('{"path":"/v1/databases/iana-http-status/dtql"}')
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(0, tool.main(["--ecb", str(ecb), "--ecb-public", str(public), "--iana", str(iana)]))
            new = (Path(d) / "public.tugql.json").read_bytes()
            self.assertEqual(b'{"workerPath": "/ecb-public/v1/databases/ecb/tugql",\n "goPath":"/v1/databases/ecb/tugql","note":"dtql stays"}', new)
            self.assertIn(hashlib.sha256(new).hexdigest(), out.getvalue())
            self.assertIn("OVDB_ECB_PUBLIC_ADMISSION_SHA256", out.getvalue())
            self.assertEqual(b'{"path":"/v1/databases/iana-http-status/tugql"}', (Path(d) / "iana.tugql.json").read_bytes())
            self.assertEqual('{"path":"/v1/databases/ecb/dtql"}', ecb.read_text())  # original untouched

    def test_refuses_a_file_that_is_not_a_legacy_admission(self):
        with tempfile.TemporaryDirectory() as d:
            already = Path(d) / "a.json"
            already.write_text('{"path":"/v1/databases/ecb/tugql"}')
            err = io.StringIO()
            with contextlib.redirect_stderr(err):
                self.assertEqual(1, tool.main(["--ecb", str(already)]))
            self.assertFalse((Path(d) / "a.tugql.json").exists())
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                tool.main([])


if __name__ == "__main__":
    unittest.main()
