"""Offline adversarial harness checks: no Docker daemon, ports or network."""
import contextlib
import io
import json
from pathlib import Path
import signal
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import Mock, patch

import prepare_fixture
import test_actual_runtime as actual


class ActualHarnessTest(unittest.TestCase):
    def test_deadline_reserves_cleanup_and_refuses_insufficient_real_expiry(self):
        commands = actual.Commands({"commands": []}, 1000)
        with patch.object(actual.time, "monotonic", return_value=500):
            self.assertEqual(440, commands.budget(530, minimum=345))
        with patch.object(actual.time, "monotonic", return_value=620):
            with self.assertRaisesRegex(RuntimeError, "insufficient deadline"):
                commands.budget(530, minimum=345)
        with patch.object(actual.time, "monotonic", return_value=970):
            self.assertEqual(15, commands.budget(15, cleanup=True))

    def test_process_timeout_kills_owned_group_and_records_actual_exit(self):
        receipt = {"commands": []}
        process = Mock(pid=1234)
        process.wait.side_effect = [subprocess.TimeoutExpired("docker", 1), -9]
        with patch.object(actual.subprocess, "Popen", return_value=process), patch.object(actual.os, "killpg") as kill, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "timed out"):
                actual.Commands(receipt, time.monotonic()+100).run(["docker", "run", "owned"], timeout=1)
        kill.assert_called_once_with(1234, signal.SIGKILL)
        self.assertEqual(-9, receipt["commands"][0]["exit"])
        self.assertEqual("failed", receipt["commands"][0]["outcome"])

    def test_nonzero_exit_cannot_be_overwritten_by_later_proxy(self):
        receipt = {"commands": []}
        process = Mock()
        process.wait.return_value = 7
        with patch.object(actual.subprocess, "Popen", return_value=process), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "phase exit 7"):
                actual.Commands(receipt, time.monotonic()+100).run(["python3", "generator", "--check"])
        self.assertEqual(7, receipt["commands"][0]["exit"])
        self.assertEqual("failed", receipt["commands"][0]["outcome"])

    def test_cleanup_retains_original_failure_and_every_owned_identity(self):
        receipt = {"outcome": "hosted_linux_candidate_pass", "primary_error": "original failure"}
        commands = Mock()
        commands.run.side_effect = subprocess.TimeoutExpired("docker", 2)
        containers = ["owned-production", "owned-workload"]
        actual.cleanup_owned(commands, containers, ["owned:image"], receipt)
        self.assertEqual("failed", receipt["outcome"])
        self.assertEqual("original failure", receipt["primary_error"])
        self.assertEqual(containers, receipt["remaining_containers"])
        self.assertEqual(3, len(receipt["cleanup_errors"]))
        self.assertFalse(receipt["cleanup_complete"])

    def test_failed_main_always_writes_receipt_when_cleanup_also_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary)/"report.json"
            def failure(_commands, _directory, _receipt, _prefix, containers, images):
                containers.append("owned-timeout")
                images.append("owned:image")
                raise RuntimeError("primary production timeout")
            with patch.dict(actual.os.environ, {"GITHUB_ACTIONS":"false", "OVDB_ACTUAL_EXPECTED_SOURCE":"", "OVDB_ACTUAL_EVENT":""}), patch.object(actual.sys, "argv", ["test", "--report", str(report)]), patch.object(actual, "experiment", side_effect=failure), patch.object(actual.Commands, "run", side_effect=["source-head", RuntimeError("inspect failed"), RuntimeError("logs failed"), RuntimeError("cleanup failed"), RuntimeError("image cleanup failed")]), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(1, actual.main())
            receipt = json.loads(report.read_bytes())
            self.assertEqual("primary production timeout", receipt["primary_error"])
            self.assertEqual("failed", receipt["outcome"])
            self.assertEqual(["owned-timeout"], receipt["remaining_containers"])
            self.assertEqual(["owned:image"], receipt["owned_images"])
            self.assertFalse(receipt["cleanup_complete"])

    def test_ballast_is_only_missing_natural_envelope(self):
        self.assertEqual(28<<20, actual.ballast_bytes(100<<20))
        self.assertEqual(0, actual.ballast_bytes(128<<20))
        for invalid in (-1, (128<<20)+1, True, 1.5):
            with self.assertRaises(ValueError):
                actual.ballast_bytes(invalid)

    def test_go_receipt_rejects_skipped_failed_or_repeated_evidence(self):
        passed = 'ACTUAL_CAPACITY_JSON={"outcome":"passed"}\n--- PASS: TestActualCapacity (1.00s)\n'
        self.assertEqual("passed", actual.parse_go_receipt(passed, "ACTUAL_CAPACITY_JSON=")["outcome"])
        for output in (passed+passed, passed+'--- SKIP: test\n', 'ACTUAL_CAPACITY_JSON={"outcome":"failed"}\n', 'PASS\n', 'ACTUAL_CAPACITY_JSON={"outcome":"passed"}\n'):
            with self.assertRaises(RuntimeError):
                actual.parse_go_receipt(output, "ACTUAL_CAPACITY_JSON=")

    def test_shared_endpoint_retains_largest_work_using_exact_go_selector(self):
        source = Path(actual.__file__).with_name("runtime_actual_linux_test.go").read_text()
        start = source.index("\n", source.index("// actualSelectionStart:")) + 1
        end = source.index("// actualSelectionEnd", start)
        selector = source[start:end]
        # Actual pinned ordering has Geo's largest table after its first table;
        # ROR's first table is its largest. Exercise the real add/largest seam,
        # including identical provider endpoints and the full selected body.
        regression = r'''
func TestSharedEndpointLargestWork(t *testing.T) {
 selection := actualWorkSelector{}
 fixtures := []struct { provider, table string; rows int64 }{
  {"geonames", "geonames_admin1", 3865},
  {"geonames", "geonames_alternate_names", 844831},
  {"ror", "locations", 141722},
  {"ror", "organizations", 141528},
  {"ror", "relationships", 72747},
 }
 for _, fixture := range fixtures {
  name := fixture.provider+"/"+fixture.table
  selection.add(fixture.provider, fixture.rows, actualWork{name:name, path:"/v1/databases/"+fixture.provider+"/dtql", body:name+"-body", want:200})
 }
 if len(selection.works)!=len(fixtures) { t.Fatalf("shared endpoints dropped selected works: %d",len(selection.works)) }
 largest:=selection.largest()
 if len(largest)!=2 || largest[0].name!="geonames/geonames_alternate_names" || largest[1].name!="ror/locations" { t.Fatalf("wrong largest works: %+v",largest) }
 if largest[0].body!="geonames/geonames_alternate_names-body" || largest[1].body!="ror/locations-body" { t.Fatal("largest request body lost") }
}
'''
        with tempfile.TemporaryDirectory() as temporary:
            probe = Path(temporary)/"selection_test.go"
            probe.write_text('package main\nimport "testing"\n'+selector+regression)
            result = subprocess.run(["go", "test", str(probe), "-run", "^TestSharedEndpointLargestWork$", "-count=1", "-v"], capture_output=True, text=True, timeout=60)
            self.assertEqual(0, result.returncode, result.stdout+result.stderr)
            self.assertIn("--- PASS: TestSharedEndpointLargestWork", result.stdout)
            self.assertNotIn("--- SKIP:", result.stdout)

    def _run_go_overlay(self, stem, helpers, helper_imports, regression, imports):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            server = Path(actual.__file__).resolve().parent
            # On Linux use the actual observer definitions. Only non-Linux
            # builds need copies of the exact extracted seams.
            helper = root / "helpers_test.go"
            helper.write_text("//go:build !linux\n\npackage main\n" + helper_imports + "\n" + helpers)
            probe = root / "regression_test.go"
            probe.write_text("package main\n" + imports + "\n" + regression)
            overlay = root / "overlay.json"
            overlay.write_text(json.dumps({"Replace": {
                str(server / (stem + "_nonlinux_test.go")): str(helper),
                str(server / (stem + "_regression_test.go")): str(probe),
            }}))
            env = dict(actual.os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
            compile_result = subprocess.run(["go", "test", "-overlay", str(overlay), "-c", "-o", str(root / "linux-observer.test"), "."], cwd=server, env=env, capture_output=True, text=True, timeout=120)
            self.assertEqual(0, compile_result.returncode, compile_result.stdout + compile_result.stderr)
            result = subprocess.run(["go", "test", "-overlay", str(overlay), ".", "-run", "^Test" + stem + "$", "-count=1", "-v"], cwd=server, capture_output=True, text=True, timeout=120)
            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            self.assertIn("--- PASS: Test" + stem, result.stdout)
            self.assertNotIn("--- SKIP:", result.stdout)

    def test_actual_manifest_read_uses_real_loader_resolved_path(self):
        source = Path(actual.__file__).with_name("runtime_actual_linux_test.go").read_text()
        start = source.index("\n", source.index("// actualManifestReadStart:")) + 1
        end = source.index("// actualManifestReadEnd", start)
        manifest_read = source[start:end]
        # Compile the actual harness seam into the native package through an
        # overlay. The existing tiny-fixture factory calls the REAL production
        # loadRuntimeInventory, including pin validation and path resolution.
        # No Docker, Linux counters, port or replica loader is involved.
        regression = r'''
func TestActualResolvedManifestRead(t *testing.T) {
 _, providers := selectedInventoryFixture(t)
 selected := 0
 for _, provider := range providers {
  if provider.ReadProfile == "" { continue }
  selected++
  if !filepath.IsAbs(provider.Manifest) { t.Fatalf("loader did not resolve %q",provider.Manifest) }
  bytes, err := actualReadResolvedManifest(provider)
  if err != nil { t.Fatalf("actual harness could not read loader-resolved manifest: %v",err) }
  if _,err := manifest.Parse(bytes); err != nil { t.Fatalf("actual resolved manifest invalid: %v",err) }
  if _,err := os.ReadFile(provider.License); err != nil { t.Fatalf("loader-resolved license invalid: %v",err) }
 }
 if selected != 2 { t.Fatalf("missing selected profile fixtures: %d",selected) }
}
'''
        self._run_go_overlay("ActualResolvedManifestRead", manifest_read, 'import "os"', regression,
                             'import ("testing"; "os"; "path/filepath"; "github.com/openvaultdb/openvaultdb-go/pkg/manifest")')

    def test_actual_generated_queries_conform_to_pinned_parser_and_handler(self):
        source = Path(actual.__file__).with_name("runtime_actual_linux_test.go").read_text()
        def seam(name):
            start = source.index("\n", source.index("// " + name + "Start:")) + 1
            return source[start:source.index("// " + name + "End", start)]
        helpers = seam("actualSelection") + seam("actualQueries")
        helpers += next(line for line in source.splitlines() if line.startswith("const actualSpoolQuery")) + "\n"
        regression = r'''
func TestActualQueryConformance(t *testing.T) {
 docs := []string{actualSpoolQuery, actualOrdinaryQuery("Order Details", 0), actualOrdinaryQuery("table's name", 1), actualSelectedQuery("table's name"), actualCallerOrderQuery("table's name", "field's name")}
 docs = append(docs, actualDiagnosticQueries("table's name", "selected's name")...)
 expectedWireQueries := map[string]struct { accept, incompleteBudget string }{
  "Sales.SalesOrderHeaderSalesReason": {accept: "application/vnd.openvaultdb.query-stream+json"},
  "Production.TransactionHistory": {accept: "application/vnd.openvaultdb.query-stream+json", incompleteBudget: "response_bytes"},
 }
 for _, work := range actualLegacyWorks() {
  if strings.HasSuffix(work.path,"/query") {
   var wire core.Query
   if err:=json.Unmarshal([]byte(work.body),&wire); err!=nil { t.Fatalf("wire query is not valid JSON: %v %s",err,work.body) }
   expected, ok := expectedWireQueries[wire.Collection]
   if !ok || work.want != 200 || work.accept != expected.accept || work.incompleteBudget != expected.incompleteBudget { t.Fatalf("wire query changed: %s %+v",work.body,work) }
   delete(expectedWireQueries,wire.Collection)
  } else { docs=append(docs,work.body) }
 }
 if len(expectedWireQueries)!=0 { t.Fatalf("wire query inventory is incomplete: %+v",expectedWireQueries) }
 for _, doc := range docs {
  if _,err := dtql.Deserialize([]byte(doc)); err!=nil { t.Fatalf("actual workload fails pinned parser: %v\n%s",err,doc) }
 }
 // Keep a control reproducing the hosted scalar orderBy rejection.
 invalid := "from: {name: table_00}\norderBy: ['native_key']\nlimit: 1000\n"
 if _,err:=dtql.Deserialize([]byte(invalid)); err==nil { t.Fatal("scalar orderBy unexpectedly accepted") }
 _, providers:=selectedInventoryFixture(t)
 var final []runtimeDatabase
 for _, p:=range providers { if p.ID=="final" { final=append(final,p) } }
 handler,closeDBs,err:=newHandlerWithProviders(final)
 if err!=nil { t.Fatal(err) }
 defer func(){ if err:=closeDBs();err!=nil { t.Error(err) } }()
 for _, table:=range final[0].SmokeRecordsets {
  query:=actualSelectedQuery(table)
  parsed,err:=dtql.Deserialize([]byte(query))
  if err!=nil || parsed.Limit()!=1000 || len(parsed.OrderBy())!=0 { t.Fatalf("selected workload bounds/order changed: %v",err) }
  response:=selectedRequest(handler,http.MethodPost,"/v1/databases/final/dtql",query)
  var page northwindQueryPage
  if response.Code!=200 || json.Unmarshal(response.Body.Bytes(),&page)!=nil || len(page.Records)!=5 { t.Fatalf("selected workload fails tiny real handler: %d %s",response.Code,response.Body.String()) }
  var expected []string
  sourceBytes,err:=os.ReadFile(final[0].Manifest)
  if err!=nil { t.Fatal(err) }
  sourceManifest,err:=manifest.Parse(sourceBytes)
  if err!=nil { t.Fatal(err) }
  helper:=sourceManifest.Storage.SQLite.RecordKeys[table]
  raw,err:=sql.Open("sqlite","file:"+filepath.Join(filepath.Dir(final[0].Manifest),"final.sqlite")+"?mode=ro")
  if err!=nil { t.Fatal(err) }
  rows,err:=raw.Query("SELECT \""+helper+"\" FROM \""+table+"\" ORDER BY \""+helper+"\" ASC LIMIT 1000")
  if err!=nil { t.Fatal(err) }
  for rows.Next() { var key string; if err:=rows.Scan(&key);err!=nil { t.Fatal(err) }; expected=append(expected,record.NewKeyWithID(table,key).String()) }
  if err:=rows.Err();err!=nil { t.Fatal(err) }; if err:=rows.Close();err!=nil { t.Fatal(err) }; if err:=raw.Close();err!=nil { t.Fatal(err) }
  if err:=actualCheckOrderedKeys(response.Body.Bytes(),expected);err!=nil { t.Fatal(err) }
  refusal:=selectedRequest(handler,http.MethodPost,"/v1/databases/final/dtql",actualCallerOrderQuery(table,"native_key"))
  if refusal.Code!=400 || !strings.Contains(refusal.Body.String(),"ordering_unsupported") { t.Fatalf("caller order accepted: %d %s",refusal.Code,refusal.Body.String()) }
  for _, diagnostic:=range actualDiagnosticQueries("hidden_table",table) { answer:=selectedRequest(handler,http.MethodPost,"/v1/databases/final/dtql",diagnostic); if answer.Code<400 || answer.Code>=500 || strings.Contains(answer.Body.String(),"unknown comparison operator") { t.Fatalf("diagnostic builder failure: %d %s",answer.Code,answer.Body.String()) } }
  ordinary:=selectedRequest(handler,http.MethodPost,"/v1/databases/final/dtql",actualOrdinaryQuery(table,1))
  if ordinary.Code!=200 { t.Fatalf("ordinary builder: %d %s",ordinary.Code,ordinary.Body.String()) }
 }
 response:=selectedRequest(handler,http.MethodPost,"/v1/databases/final/dtql",invalid)
 if response.Code!=400 || !strings.Contains(response.Body.String(),"invalid_dtql") { t.Fatalf("invalid scalar order not rejected before reads: %d %s",response.Code,response.Body.String()) }
}
'''
        self._run_go_overlay("ActualQueryConformance", helpers, 'import ("fmt"; "strings"; "encoding/json")', regression,
                             'import ("testing"; "strings"; "encoding/json"; "net/http"; "github.com/dal-go/dalgo/dtql"; "github.com/openvaultdb/openvaultdb-go/pkg/core"; "database/sql"; "os"; "path/filepath"; "github.com/openvaultdb/openvaultdb-go/pkg/manifest"; "github.com/dal-go/record")')

    def test_exact_workflow_head_refuses_merge_head_before_docker(self):
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary)/"report.json"
            with patch.dict(actual.os.environ, {"GITHUB_ACTIONS":"true", "OVDB_ACTUAL_EXPECTED_SOURCE":"b"*40}), patch.object(actual.sys, "argv", ["test", "--report", str(report)]), patch.object(actual.Commands, "run", return_value="a"*40), patch.object(actual, "experiment") as experiment, contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(1, actual.main())
                experiment.assert_not_called()
            receipt = json.loads(report.read_bytes())
            self.assertEqual("a"*40, receipt["source_head"])
            self.assertEqual("b"*40, receipt["expected_source_head"])
            self.assertEqual("failed", receipt["outcome"])
            self.assertIn("exact workflow source head", receipt["primary_error"])
            self.assertTrue(receipt["cleanup_complete"])

    def test_hosted_source_binding_rejects_missing_or_noncanonical_revision(self):
        with patch.dict(actual.os.environ, {"GITHUB_ACTIONS":"true"}):
            for expected in ("", "main", "A"*40):
                with self.assertRaises(ValueError):
                    actual.validate_source_head("a"*40, expected)
            actual.validate_source_head("a"*40, "a"*40)

    def test_native_proof_covers_diagnostic_values_not_just_selected_helpers(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            before = root/"before.sqlite"
            with sqlite3.connect(before) as connection:
                connection.executescript("CREATE TABLE selected(id TEXT PRIMARY KEY, value TEXT); INSERT INTO selected VALUES('key','original'); CREATE TABLE diagnostic(id TEXT, value TEXT); INSERT INTO diagnostic VALUES('hidden','original');")
            prepare_fixture.prepare(before, root/"output", "candidate", prepare_fixture.sha256_file(before), "natural", serving_adapter="separate-id/1", selected_tables=["selected"])
            after = root/"output"/"candidate.sqlite"
            proof = actual.native_proof(before, after, ["selected"])
            self.assertEqual(2, len(proof))
            self.assertEqual(1, sum(table.get("keys_scanned", 0) for table in proof))
            with sqlite3.connect(after) as connection:
                connection.execute("UPDATE diagnostic SET value='changed'")
            with self.assertRaisesRegex(ValueError, "typed native values changed"):
                actual.native_proof(before, after, ["selected"])


if __name__ == "__main__":
    unittest.main()
