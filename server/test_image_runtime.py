"""Finite Linux/Docker synthetic experiment; no ports, secrets, corpus or deployment.

Requires an already-running Linux Docker engine. Never starts/installs a daemon.
Uses the production Dockerfile and guard, and records final merged image metadata.
"""
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid

import test_selected_runtime
import prepare_providers


def run(arguments, *, timeout=180, input_data=None, environment=None):
    print("+", " ".join(map(str, arguments)), flush=True)
    result = subprocess.run(arguments, input=input_data, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=timeout, env=environment)
    if result.stderr:
        print(result.stderr.decode(errors="replace"), end="", flush=True)
    if result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): {arguments[0]}\n"
                           + result.stdout.decode(errors="replace"))
    return result.stdout


def validate_export(data):
    """Inspect the final merged filesystem, not historical hidden layer entries."""
    entries = []
    with tarfile.open(fileobj=io.BytesIO(data)) as archive:
        for member in archive:
            name = member.name.lstrip("./").rstrip("/")
            if name in ("srv", "srv/fixture") or name.startswith("srv/fixture/"):
                if member.uid != 0 or member.gid != 0:
                    raise RuntimeError("non-root final image entry: " + name)
                if member.isdir():
                    if member.mode & 0o022:
                        raise RuntimeError("writable final image directory: " + name)
                elif not member.isfile() or member.mode != 0o444:
                    raise RuntimeError("final image fixture file is not regular 0444: " + name)
                entries.append({"path": "/" + name, "mode": oct(member.mode),
                                "uid": member.uid, "gid": member.gid,
                                "type": "directory" if member.isdir() else "file",
                                "bytes": member.size})
            if name == "tmp":
                if not member.isdir() or member.mode != 0o1777:
                    raise RuntimeError("scratch /tmp must be a sticky writable directory")
    if not any(e["path"] == "/srv/fixture/inventory.json" for e in entries):
        raise RuntimeError("final image has no fixed inventory")
    return entries


def hostile_export(data, mutation):
    output = io.BytesIO()
    with tarfile.open(fileobj=io.BytesIO(data)) as source, tarfile.open(fileobj=output, mode="w") as target:
        for original in source:
            member = copy.copy(original)
            payload = source.extractfile(original).read() if original.isfile() else None
            name = member.name.lstrip("./").rstrip("/")
            if name == "srv" and mutation == "ancestor":
                member.mode = 0o777
            if name == "srv/fixture/inventory.json":
                if mutation == "writable": member.mode = 0o666
                if mutation == "owner": member.uid = member.gid = 65532
                if mutation == "pin":
                    document = json.loads(payload)
                    document["databases"][0]["servingSha256"] = "0" * 64
                    payload = json.dumps(document).encode()
            if name == "srv/fixture/candidate.sqlite":
                if mutation == "truncated": payload = payload[:10]
                if mutation == "wal-header": payload = payload[:18] + b"\x02\x02" + payload[20:]
            if payload is not None:
                member.size = len(payload)
            target.addfile(member, io.BytesIO(payload) if payload is not None else None)
        if mutation in ("symlink", "hardlink", "fifo", "wal-sidecar"):
            member = tarfile.TarInfo("srv/fixture/hostile")
            member.uid = member.gid = 0
            member.mode = 0o444
            if mutation == "symlink":
                member.type, member.linkname = tarfile.SYMTYPE, "candidate.sqlite"
            elif mutation == "hardlink":
                member.type, member.linkname = tarfile.LNKTYPE, "srv/fixture/candidate.sqlite"
            elif mutation == "fifo": member.type = tarfile.FIFOTYPE
            else: member.name = "srv/fixture/candidate.sqlite-wal"
            target.addfile(member)
    return output.getvalue()


def main():
    report_path = Path(os.getenv("OVDB_IMAGE_REPORT", "image-runtime-report.json")).resolve()
    receipt = {"experiment": "tiny Linux protected-image runtime", "actual_corpus": False,
               "capacity_accepted": False, "platform_accepted": False, "commands": [],
               "source_head": run(["git", "rev-parse", "HEAD"]).decode().strip()}
    prefix = "ovdb-image-test-" + uuid.uuid4().hex[:12]
    images = []
    container = None
    try:
        # This fails clearly when unavailable; it never converts missing Docker into a skip.
        engine = json.loads(run(["docker", "info", "--format", "{{json .}}"], timeout=30))
        if engine.get("OSType") != "linux": raise RuntimeError("image experiment requires an existing Linux Docker engine")
        receipt["docker_server"] = {k: engine.get(k) for k in ("ServerVersion", "OSType", "Architecture")}
        with tempfile.TemporaryDirectory(prefix=prefix) as temporary:
            directory = Path(temporary)
            context = directory / "context"
            context.mkdir()
            generated = directory / "generated"
            generated.mkdir()
            inventory = test_selected_runtime.build_inventory(generated)
            prepare_providers.validate_image_layout(inventory.parent)
            shutil.copytree(inventory.parent, context / "fixture" / "output")
            shutil.copy("fixture/UPSTREAM-LICENSE.md", context / "fixture" / "UPSTREAM-LICENSE.md")
            shutil.copy("Dockerfile", context / "Dockerfile")
            shutil.copytree("image-layout", context / "image-layout")
            environment = {**os.environ, "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
            run(["go", "build", "-trimpath", "-o", str(context / "ovdb-cloud"), "."], timeout=300, environment=environment)
            run(["go", "test", "-c", "-o", str(context / "image-test"), "."], timeout=300, environment=environment)
            base = prefix + ":production-layout"
            images.append(base)
            run(["docker", "build", "--platform=linux/amd64", "-t", base, str(context)], timeout=300)
            receipt["production_image"] = json.loads(run(["docker", "image", "inspect", base]))[0]
            (context / "Dockerfile.test").write_text(f"FROM {base}\nCOPY --chown=0:0 --chmod=0555 image-test /srv/image-test\nENTRYPOINT [\"/srv/image-test\"]\n")
            accepted = prefix + ":accepted"
            images.append(accepted)
            run(["docker", "build", "--platform=linux/amd64", "-f", str(context / "Dockerfile.test"), "-t", accepted, str(context)], timeout=120)
            container = run(["docker", "create", accepted]).decode().strip()
            exported = run(["docker", "export", container])
            receipt["final_entries"] = validate_export(exported)
            receipt["export_sha256"] = hashlib.sha256(exported).hexdigest()
            run(["docker", "rm", container]); container = None
            def exercise(image, mode, expected="", extra=()):
                started = time.monotonic()
                arguments = ["docker", "run", "--rm", "--network=none", "--memory=512m", "--cpus=1", *extra,
                             "-e", "OVDB_IMAGE_TEST=" + mode, "-e", "OVDB_IMAGE_EXPECT=" + expected,
                             image, "-test.run=^TestProtectedImageLinuxJourney$", "-test.v", "-test.timeout=60s"]
                output = run(arguments, timeout=90).decode()
                print(output, end="", flush=True)
                if "--- PASS: TestProtectedImageLinuxJourney" not in output or "--- SKIP:" in output:
                    raise RuntimeError("image experiment did not execute successfully")
                receipt["commands"].append({"argv": arguments, "seconds": time.monotonic()-started, "output": output, "exit": 0})
            exercise(accepted, "accept")
            mutations = {"ancestor": "directory", "writable": "writable, linked", "owner": "not root-owned",
                         "symlink": "writable, linked", "hardlink": "writable, linked", "fifo": "writable, linked",
                         "wal-sidecar": "sidecar", "wal-header": "checkpointed", "truncated": "EOF", "pin": "hash does not match"}
            for mutation, expected in mutations.items():
                image = prefix + ":" + mutation
                images.append(image)
                run(["docker", "import", "--change", "USER 65532:65532", "--change", "ENV OVDB_SELECTED_STORAGE=protected-image",
                     "--change", 'ENTRYPOINT ["/srv/image-test"]', "-", image], input_data=hostile_export(exported, mutation))
                exercise(image, "refuse", expected)
            exercise(accepted, "refuse", "actual UID/GID", ("--user=0:0",))
            exercise(accepted, "refuse", "overlaps protected image",
                     ("--mount", f"type=bind,src={context / 'fixture' / 'output'},dst=/srv/fixture,readonly",))
            receipt["outcome"] = "synthetic_image_pass"
    except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
        receipt["outcome"], receipt["error"] = "failed", str(error)
        print("Linux image experiment FAILED:", error, file=sys.stderr)
        return 1
    finally:
        if container:
            subprocess.run(["docker", "rm", "-f", container], timeout=30, check=False)
        if images:
            subprocess.run(["docker", "image", "rm", "-f", *images], timeout=60, check=False)
        report_path.write_text(json.dumps(receipt, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
