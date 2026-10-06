"""Manual full-corpus Linux experiment, never a cold Cloud capacity acceptance.

Run from server/ with an existing Linux Docker engine. No daemon creation, host
ports, credentials or deploy. The hard 18-minute deadline reserves two minutes
of the 20-minute job for cleanup/upload. Host preparation may need 4 GiB disk;
only container memory is capacity evidence. All temporary artifacts are owned.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import signal
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import uuid

import prepare_fixture
import prepare_providers

# Only reviewed, publicly merged wrapper revisions are admissible here.
W1_REVISIONS = {"ingitdb/geo-ingitdb": "d53aa273448e99e792899340eb4c162e67834b69",
                "ingitdb/ror-ingitdb": "49bc19225c1f2c4e96fbf414bf2359b4550e1f17"}
ENVELOPE_BYTES = 128 << 20
PEAK_BYTES = 435 << 20
SCRIPT_SECONDS = 1080
CLEANUP_SECONDS = 60
MAX_LOG_BYTES = 1 << 20


class Commands:
    """Finite process groups, individual exit receipts, bounded output tails."""
    def __init__(self, receipt, deadline):
        self.receipt, self.deadline = receipt, deadline

    def budget(self, maximum, minimum=1, cleanup=False):
        remaining = self.deadline - time.monotonic() - (0 if cleanup else CLEANUP_SECONDS)
        if remaining < minimum:
            raise RuntimeError(f"insufficient deadline budget: {remaining:.2f}s; need {minimum}s")
        return min(maximum, remaining)

    def run(self, argv, timeout=180, minimum=1, environment=None, cleanup=False):
        bound = self.budget(timeout, minimum, cleanup)
        command = {"argv": list(map(str, argv)), "timeout_seconds": bound,
                   "exit": None, "outcome": "failed"}
        self.receipt["commands"].append(command)
        started = time.monotonic()
        print("+", " ".join(command["argv"]), flush=True)
        with tempfile.TemporaryFile() as output:
            process = None
            try:
                process = subprocess.Popen(command["argv"], stdout=output, stderr=subprocess.STDOUT,
                                           env=environment, start_new_session=True)
                try:
                    command["exit"] = process.wait(timeout=bound)
                except subprocess.TimeoutExpired:
                    command["timed_out"] = True
                    os.killpg(process.pid, signal.SIGKILL)
                    command["exit"] = process.wait(timeout=5)
                    raise RuntimeError(f"phase timed out after {bound:.2f}s: {argv[0]}")
                if command["exit"] != 0:
                    raise RuntimeError(f"phase exit {command['exit']}: {argv[0]}")
                command["outcome"] = "passed"
            finally:
                command["seconds"] = time.monotonic() - started
                output.seek(0, 2)
                command["output_bytes"] = output.tell()
                output.seek(max(0, command["output_bytes"] - MAX_LOG_BYTES))
                command["output_tail"] = output.read().decode(errors="replace")
                print(command["output_tail"], end="", flush=True)
        return command["output_tail"]


def ballast_bytes(retained):
    if type(retained) is not int or retained < 0 or retained > ENVELOPE_BYTES:
        raise ValueError("retained natural snapshot bytes outside two-slot envelope")
    return ENVELOPE_BYTES - retained


def fetch_metadata(repository, revision, path):
    return prepare_providers.default_fetch(repository, revision, path)


def make_inventory(fetch=fetch_metadata):
    inventory = prepare_providers.read_json(Path("providers.json").read_bytes(), "legacy inventory")
    if inventory["version"] != 1 or len(inventory["databases"]) != 6:
        raise ValueError("requires unchanged complete six-provider inventory")
    inventory = copy.deepcopy(inventory)
    inventory["version"] = 2
    names = {"manifest": "manifest.json", "contract": "metadata/contract.json",
             "checksums": "metadata/checksums.json", "databaseManifest": "ovdb-database.json",
             "license": "DATA-LICENSE.md"}
    for repository, revision in W1_REVISIONS.items():
        if not prepare_providers.COMMIT_PATTERN.fullmatch(revision):
            raise ValueError("W1 wrapper has no exact reviewed public revision")
        publisher_bytes = fetch(repository, revision, "ovdb.yaml")
        publisher = prepare_providers.parse_publisher(publisher_bytes)
        files = {}
        for role, path in names.items():
            data = fetch(repository, revision, path)
            files[role] = {"path": path, "sha256": prepare_providers.sha256(data), "bytes": len(data)}
        artifact = prepare_providers.read_json(fetch(repository, revision, "metadata/artifact.json"), "artifact")
        files["artifact"] = artifact["sqlite"]
        inventory["databases"].append({"id": publisher["id"], "repository": repository,
            "revision": revision, "recordKeyFormat": "natural", "requirePublishedQuery": False,
            "servingAdapter": "separate-id/1", "readProfile": "bounded-immutable/1",
            "publisherManifest": {"path": "ovdb.yaml", "sha256": prepare_providers.sha256(publisher_bytes),
                                  "bytes": len(publisher_bytes)},
            "files": files, "corsOrigins": ["https://demodb.dev"]})
    prepare_providers.validate_inventory(inventory)
    return inventory


def native_proof(before_path, after_path, selected):
    """All native values/schema plus every helper key; never a row sample."""
    before = sqlite3.connect(before_path.as_uri() + "?mode=ro", uri=True)
    after = sqlite3.connect(after_path.as_uri() + "?mode=ro", uri=True)
    try:
        names = [r[0] for r in before.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
        if names != [r[0] for r in after.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]:
            raise ValueError("native table set changed")
        if list(before.execute("SELECT name,sql FROM sqlite_master WHERE type='view' ORDER BY name")) != list(after.execute("SELECT name,sql FROM sqlite_master WHERE type='view' ORDER BY name")):
            raise ValueError("native views changed")
        results = []
        for table in names:
            quoted = prepare_fixture.quote_identifier(table)
            columns = list(before.execute(f"PRAGMA table_xinfo({quoted})"))
            original = {r[1] for r in columns}
            added = {r[1] for r in after.execute(f"PRAGMA table_xinfo({quoted})")} - original
            if len(added) != int(table in selected):
                raise ValueError("selected-only helper admission mismatch")
            digest = prepare_fixture.native_value_digest(before, table, columns)
            if digest != prepare_fixture.native_value_digest(after, table, columns):
                raise ValueError("typed native values changed")
            count = before.execute(f"SELECT count(*) FROM {quoted}").fetchone()[0]
            if count != after.execute(f"SELECT count(*) FROM {quoted}").fetchone()[0]:
                raise ValueError("native row count changed")
            entry = {"table": table, "rows": count, "selected": table in selected,
                     "typed_native_value_sha256": digest}
            ignore = None
            if added:
                helper = next(iter(added))
                indexes = {r[1] for r in after.execute(f"PRAGMA index_list({quoted})")} - {r[1] for r in before.execute(f"PRAGMA index_list({quoted})")}
                if len(indexes) != 1:
                    raise ValueError("helper index admission mismatch")
                ignore = (helper, next(iter(indexes)))
                key = prepare_fixture.quote_identifier(helper)
                keys, stream, previous = 0, hashlib.sha256(), None
                for value, kind in after.execute(f"SELECT {key},typeof({key}) FROM {quoted} ORDER BY {key}"):
                    if kind != "text" or not isinstance(value, str) or not value or value == previous:
                        raise ValueError("invalid or repeated helper key")
                    encoded = value.encode("utf-8", errors="strict")
                    stream.update(len(encoded).to_bytes(8, "big") + encoded)
                    keys, previous = keys + 1, value
                if keys != count:
                    raise ValueError("incomplete helper key scan")
                entry.update(helper=helper, keys_scanned=keys, key_stream_sha256=stream.hexdigest())
            if prepare_fixture.table_metadata(before, table) != prepare_fixture.table_metadata(after, table, ignore):
                raise ValueError("native columns/foreign keys/indexes changed")
            results.append(entry)
        return results
    finally:
        before.close()
        after.close()


def prepare_phase(directory):
    started = time.monotonic()
    inventory = make_inventory()
    pins = directory / "providers-test.json"
    pins.write_text(json.dumps(inventory, indent=2) + "\n")
    output = directory / "fixture" / "output"
    proof = []
    original_prepare = prepare_fixture.prepare

    def checked_prepare(source, destination, database_id, *args, **kwargs):
        # Test-only host copy retains source until independent complete comparison;
        # production preparation and final serving bytes are unchanged.
        selected = kwargs.get("selected_tables")
        kwargs["consume_source"] = False
        result = original_prepare(source, destination, database_id, *args, **kwargs)
        serving = destination / (database_id + ".sqlite")
        if selected is not None:
            proof.append({"id": database_id, "tables": native_proof(source.resolve(), serving.resolve(), selected)})
        return result

    prepare_fixture.prepare = checked_prepare
    try:
        prepare_providers.prepare_inventory(pins, output)
    finally:
        prepare_fixture.prepare = original_prepare
    prepare_providers.validate_image_layout(output)
    native = sum(len(p["tables"]) for p in proof)
    selected = sum(t["selected"] for p in proof for t in p["tables"])
    keys = sum(t.get("keys_scanned", 0) for p in proof for t in p["tables"])
    if (native, selected, keys) != (16, 11, 1280314):
        raise ValueError(f"full corpus mismatch: {native}/{selected}/{keys}")
    receipt = {"outcome": "passed", "seconds": time.monotonic() - started,
               "inventory": inventory, "native_tables": native, "selected_tables": selected,
               "keys_scanned": keys, "native_proof": proof,
               "prepared_files": [{"path": p.name, "bytes": p.stat().st_size,
                                   "sha256": prepare_fixture.sha256_file(p)} for p in sorted(output.iterdir())]}
    (directory / "preparation.json").write_text(json.dumps(receipt, indent=2) + "\n")


def parse_go_receipt(output, marker):
    lines = [line[len(marker):] for line in output.splitlines() if line.startswith(marker)]
    expected = {"ACTUAL_CAPACITY_JSON=": "TestActualCapacity", "ACTUAL_PROBE_JSON=": "TestActualProductionProbe"}.get(marker)
    if not expected or len(lines) != 1 or "--- SKIP:" in output or f"--- PASS: {expected} " not in output:
        raise RuntimeError("missing, repeated or skipped actual experiment receipt")
    receipt = json.loads(lines[0])
    if receipt.get("outcome") != "passed":
        raise RuntimeError("actual Go experiment did not pass")
    return receipt



def validate_source_head(actual, expected):
    if expected and (not prepare_providers.COMMIT_PATTERN.fullmatch(expected) or actual != expected):
        raise ValueError("checked-out source differs from exact workflow source head")
    if os.getenv("GITHUB_ACTIONS") == "true" and not expected:
        raise ValueError("hosted experiment lacks exact workflow source-head binding")

def cleanup_owned(commands, containers, images, receipt):
    errors = receipt.setdefault("cleanup_errors", [])
    for name in list(containers):
        # Capture terminal state/OOM and logs even when startup/workload failed.
        for command in (["docker", "inspect", name], ["docker", "logs", "--tail", "30", name]):
            try:
                output = commands.run(command, timeout=5, cleanup=True)
                if command[1] == "inspect":
                    receipt.setdefault("owned_container_states", {})[name] = json.loads(output)[0]
            except Exception as error:
                receipt.setdefault("recovery_observation_errors", []).append(str(error))
        try:
            commands.run(["docker", "rm", "-f", name], timeout=15, cleanup=True)
            containers.remove(name)
        except Exception as error:  # Every owned resource and the original error survive.
            errors.append({"container": name, "error": str(error)})
    if images:
        try:
            commands.run(["docker", "image", "rm", "-f", *images], timeout=20, cleanup=True)
        except Exception as error:
            errors.append({"images": list(images), "error": str(error)})
    receipt["remaining_containers"] = list(containers)
    receipt["cleanup_complete"] = not containers and not errors
    if not receipt["cleanup_complete"]:
        receipt["outcome"] = "failed"


def experiment(commands, directory, receipt, prefix, containers, images):
    engine = json.loads(commands.run(["docker", "info", "--format", "{{json .}}"], timeout=20))
    if engine.get("OSType") != "linux":
        raise RuntimeError("requires an existing Linux Docker engine; never starts a daemon")
    receipt["docker_server"] = {k: engine.get(k) for k in ("ServerVersion", "OSType", "Architecture")}
    if shutil.disk_usage(directory).free < 4 << 30:
        raise RuntimeError("host preparation requires 4 GiB available scratch disk")
    commands.run([sys.executable, __file__, "--prepare-phase", str(directory)], timeout=180)
    receipt["preparation"] = json.loads((directory / "preparation.json").read_bytes())
    shutil.copy("Dockerfile", directory / "Dockerfile")
    shutil.copy("fixture/UPSTREAM-LICENSE.md", directory / "fixture" / "UPSTREAM-LICENSE.md")
    shutil.copytree("image-layout", directory / "image-layout")
    environment = {**os.environ, "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
    commands.run(["go", "build", "-trimpath", "-o", str(directory / "ovdb-cloud"), "."], timeout=120, environment=environment)
    commands.run(["go", "test", "-c", "-o", str(directory / "actual-test"), "."], timeout=120, environment=environment)
    base, test = prefix + ":production", prefix + ":observer"
    images.extend([base, test])
    commands.run(["docker", "build", "--platform=linux/amd64", "-t", base, str(directory)], timeout=120)
    receipt["production_image"] = json.loads(commands.run(["docker", "image", "inspect", base], timeout=10))[0]
    (directory / "Dockerfile.test").write_text(f"FROM {base}\nCOPY --chown=0:0 --chmod=0555 actual-test /srv/actual-test\n")
    commands.run(["docker", "build", "--platform=linux/amd64", "-f", str(directory / "Dockerfile.test"), "-t", test, str(directory)], timeout=60)
    receipt["observer_image"] = json.loads(commands.run(["docker", "image", "inspect", test], timeout=10))[0]
    limits = ["--network=none", "--memory=512m", "--memory-swap=512m", "--cpus=1",
              "--tmpfs", "/tmp:rw,size=256m,mode=1777", "--cap-drop=ALL", "--security-opt=no-new-privileges"]
    production = prefix + "-production"
    containers.append(production)
    receipt["owned_containers"].append(production)
    started = time.monotonic()
    commands.run(["docker", "run", "-d", "--name", production, *limits, test], timeout=10)
    receipt["production_start_identity"] = json.loads(commands.run(["docker", "inspect", production], timeout=10))[0]
    probe = commands.run(["docker", "exec", "-e", "OVDB_ACTUAL_PROBE=1", production,
                          "/srv/actual-test", "-test.run=^TestActualProductionProbe$", "-test.v", "-test.timeout=230s"], timeout=230)
    receipt["production_probe"] = parse_go_receipt(probe, "ACTUAL_PROBE_JSON=")
    elapsed = time.monotonic() - started
    receipt["production_startup_seconds"] = elapsed
    receipt["startup_probe_ceiling_seconds"] = 240
    receipt["startup_margin_seconds"] = 240 - elapsed
    if elapsed >= 240:
        raise RuntimeError("actual executable readiness exceeded the 240s probe ceiling")
    commands.run(["docker", "stop", "-t", "10", production], timeout=15)
    production_info = json.loads(commands.run(["docker", "inspect", production], timeout=10))[0]
    receipt.setdefault("owned_container_states", {})[production] = production_info
    receipt["production_state"] = production_info["State"]
    if receipt["production_state"]["OOMKilled"] or receipt["production_state"]["ExitCode"] != 0:
        raise RuntimeError("production failed shutdown or OOMed")
    commands.run(["docker", "rm", production], timeout=10)
    containers.remove(production)
    workload = prefix + "-workload"
    containers.append(workload)
    receipt["owned_containers"].append(workload)
    # Detached start permits exact live host PID/CID binding before observing
    # logs. The Go result and final exit/OOM state are independently required.
    commands.budget(530, minimum=345)
    commands.run(["docker", "create", "--name", workload, *limits, "-e", "OVDB_ACTUAL_CAPACITY=1",
                  "--entrypoint=/srv/actual-test", test, "-test.run=^TestActualCapacity$",
                  "-test.v", "-test.timeout=510s"], timeout=10)
    commands.run(["docker", "start", workload], timeout=10)
    receipt["workload_start_identity"] = json.loads(commands.run(["docker", "inspect", workload], timeout=10))[0]
    if not receipt["workload_start_identity"]["State"]["Running"] or receipt["workload_start_identity"]["State"]["Pid"] <= 0:
        raise RuntimeError("workload has no live stable PID/cgroup identity")
    output = commands.run(["docker", "logs", "-f", workload], timeout=530, minimum=345)
    receipt["workload"] = parse_go_receipt(output, "ACTUAL_CAPACITY_JSON=")
    workload_info = json.loads(commands.run(["docker", "inspect", workload], timeout=10))[0]
    receipt.setdefault("owned_container_states", {})[workload] = workload_info
    receipt["workload_state"] = workload_info["State"]
    if receipt["workload_state"]["OOMKilled"] or receipt["workload_state"]["ExitCode"] != 0:
        raise RuntimeError("workload container failed or OOMed")
    receipt["outcome"] = "hosted_linux_candidate_pass"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prepare-phase", type=Path)
    parser.add_argument("--report", type=Path, default=Path("actual-runtime-report.json"))
    args = parser.parse_args()
    if args.prepare_phase:
        prepare_phase(args.prepare_phase.resolve())
        return 0
    receipt = {"outcome": "failed", "commands": [], "owned_containers": [],
               "actual_corpus": True, "cold_cloud_capacity_accepted": False,
               "public_provider_admission": False, "transport_accepted": False,
               "capacity_peak_limit_bytes": PEAK_BYTES, "retained_envelope_bytes": ENVELOPE_BYTES,
               "caveat": "Shared warmed image pages can be charged outside Docker cgroup; cold Cloud proof remains mandatory."}
    started = time.monotonic()
    available = SCRIPT_SECONDS
    # Workflow records its start before checkout/setup so slow prerequisites
    # cannot consume the cleanup/upload reservation unnoticed.
    if "OVDB_ACTUAL_JOB_STARTED" in os.environ:
        try:
            elapsed = max(0, time.time() - float(os.environ["OVDB_ACTUAL_JOB_STARTED"]))
            available = min(available, 1200 - elapsed - 60)
        except ValueError:
            available = 0
    receipt["overall_script_budget_seconds"] = available
    commands = Commands(receipt, started + available)
    prefix = "ovdb-actual-" + uuid.uuid4().hex[:12]
    images, containers = [], []
    try:
        receipt["source_head"] = commands.run(["git", "rev-parse", "HEAD"], timeout=10).strip()
        receipt["expected_source_head"] = os.getenv("OVDB_ACTUAL_EXPECTED_SOURCE", "")
        receipt["workflow_event"] = os.getenv("OVDB_ACTUAL_EVENT", "")
        validate_source_head(receipt["source_head"], receipt["expected_source_head"])
        with tempfile.TemporaryDirectory(prefix=prefix) as temporary:
            experiment(commands, Path(temporary), receipt, prefix, containers, images)
    except Exception as error:
        receipt["outcome"], receipt["primary_error"] = "failed", str(error)
        print("Actual capacity experiment FAILED:", error, file=sys.stderr)
    finally:
        try:
            cleanup_owned(commands, containers, images, receipt)
        finally:
            receipt["owned_images"] = list(images)
            receipt["seconds"] = time.monotonic() - started
            args.report.resolve().write_text(json.dumps(receipt, indent=2) + "\n")
    return 0 if receipt["outcome"] == "hosted_linux_candidate_pass" else 1


if __name__ == "__main__":
    sys.exit(main())
