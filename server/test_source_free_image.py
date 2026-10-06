"""Finite actual-shipping-image probes; never prepares or reads provider data.

The default run proves readable roots and the process-only dynamic composition.
Public-root TLS remains unproved unless the operator explicitly supplies its
authored HTTPS positive endpoint/digest and a certificate-invalid negative peer.
Every container/image is named, owned by this invocation and removed in finally.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import tempfile
import uuid


def run(arguments, *, cwd=None, timeout=60, env=None):
    return subprocess.run(arguments, cwd=cwd, env=env, capture_output=True,
                          text=True, timeout=timeout, check=False)


def checked_metadata(kind, evidence):
    fields = {
        "shipping_roots": {"probe", "outcome", "rootsSha256", "certificates"},
        "synthetic_dynamic": {"probe", "outcome", "publicAdmission", "networkCalls", "syntheticReads", "evidenceSha256"},
        "shipping_root_https": {"probe", "outcome", "rootsSha256", "bytes", "bodySha256"},
        "invalid_certificate": {"probe", "outcome", "reason", "rootsSha256"},
    }
    probes = {"shipping_roots": "roots/1", "synthetic_dynamic": "synthetic-dynamic/1",
              "shipping_root_https": "https-trust/1", "invalid_certificate": "https-trust/1"}
    if not isinstance(evidence, dict) or set(evidence) != fields[kind] or evidence.get("probe") != probes[kind]:
        raise RuntimeError(kind + "_metadata_shape_invalid")
    for key in ("rootsSha256", "evidenceSha256", "bodySha256"):
        if key in evidence and (not isinstance(evidence[key], str) or re.fullmatch("[a-f0-9]{64}", evidence[key]) is None):
            raise RuntimeError(kind + "_digest_invalid")
    for key in ("certificates", "bytes", "networkCalls", "syntheticReads"):
        if key in evidence and (type(evidence[key]) is not int or not 0 <= evidence[key] <= 4096):
            raise RuntimeError(kind + "_count_invalid")
    if kind == "synthetic_dynamic" and (evidence["publicAdmission"] is not False or evidence["networkCalls"] != 0 or evidence["syntheticReads"] != 1):
        raise RuntimeError("synthetic_boundary_invalid")
    return evidence


def experiment(receipt, options, owned):
    root = Path(__file__).resolve().parent
    # Fail before creating resources when the engine is unavailable.
    if run(["docker", "info", "--format", "{{.ServerVersion}}"], timeout=10).returncode:
        raise RuntimeError("docker_engine_unavailable")
    with tempfile.TemporaryDirectory(prefix="ovdb-source-free-") as temporary:
        context = Path(temporary)
        environment = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
        result = run(["go", "build", "-trimpath", "-o", str(context / "ovdb-cloud"), "."],
                     cwd=root, env=environment, timeout=180)
        if result.returncode:
            raise RuntimeError("shipping_binary_build_failed")
        receipt["binary_sha256"] = hashlib.sha256((context / "ovdb-cloud").read_bytes()).hexdigest()
        shutil.copy(root / "Dockerfile", context / "Dockerfile")
        shutil.copytree(root / "image-layout", context / "image-layout")
        (context / "fixture/output").mkdir(parents=True)
        # Authored empty metadata: no source fixture, snapshot, provider profile.
        (context / "fixture/output/probe-only.json").write_text("{}\n")
        (context / "fixture/UPSTREAM-LICENSE.md").write_text("Synthetic probe context only\n")
        result = run(["docker", "build", "--platform", "linux/amd64", "-t", owned["image"], str(context)], timeout=180)
        if result.returncode:
            raise RuntimeError("shipping_image_build_failed")
        owned["built"] = True
        identity = run(["docker", "image", "inspect", owned["image"], "--format", "{{.Id}}"], timeout=10)
        if identity.returncode:
            raise RuntimeError("image_identity_failed")
        receipt["image_id"] = identity.stdout.strip()
        for kind, args, network, accepted in [
            ("shipping_roots", ["--roots-probe"], "none", True),
            ("synthetic_dynamic", ["--synthetic-dynamic-probe"], "none", True),
        ] + ([
            ("shipping_root_https", ["--https-trust-probe", options.https_url, options.expected_sha256], "bridge", True),
            ("invalid_certificate", ["--https-trust-probe", options.invalid_https_url, options.expected_sha256], "bridge", False),
        ] if options.https_url else []):
            name = owned["prefix"] + "-" + kind.replace("_", "-")
            owned["containers"].append(name)
            result = run(["docker", "run", "--name", name, "--platform", "linux/amd64",
                          "--network", network, "--read-only", "--cap-drop=ALL",
                          "--security-opt=no-new-privileges", owned["image"], *args], timeout=30)
            try:
                evidence = json.loads(result.stdout)
            except ValueError:
                raise RuntimeError(kind + "_receipt_invalid") from None
            evidence = checked_metadata(kind, evidence)
            if accepted:
                if result.returncode or evidence.get("outcome") != "pass":
                    raise RuntimeError(kind + "_failed")
            elif result.returncode == 0 or evidence.get("outcome") != "refused" or evidence.get("reason") != "tls_validation_failed":
                raise RuntimeError("invalid_certificate_not_refused_by_tls")
            receipt["checks"][kind] = evidence
        receipt["outcome"] = "source_free_image_subset_pass"
        receipt["shipping_public_root_tls_verified"] = bool(options.https_url)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", default="source-free-image-report.json")
    parser.add_argument("--https-url", default="")
    parser.add_argument("--expected-sha256", default="")
    parser.add_argument("--invalid-https-url", default="")
    options = parser.parse_args()
    if any((options.https_url, options.expected_sha256, options.invalid_https_url)) and not all((options.https_url, options.expected_sha256, options.invalid_https_url)):
        parser.error("public-root TLS checks require both authored endpoints and the expected synthetic digest")
    prefix = "ovdb-source-free-" + uuid.uuid4().hex[:12]
    owned = {"prefix": prefix, "image": prefix + ":shipping", "containers": [], "built": False}
    receipt = {"format": "ovdb-source-free-image/1", "owner": prefix, "outcome": "failed",
               "checks": {}, "shipping_public_root_tls_verified": False,
               "public_dynamic_inventory_admitted": False, "deployed_chain_cancellation_verified": False,
               "provider_profile_configured": False, "provider_egress_independently_verified": False,
               "cleanup_complete": False}
    try:
        experiment(receipt, options, owned)
    except (RuntimeError, subprocess.TimeoutExpired, OSError) as error:
        # Closed diagnostics; never preserve command response/error payloads.
        receipt["failure"] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
    finally:
        clean = True
        for name in owned["containers"]:
            try:
                clean = run(["docker", "rm", "-f", name], timeout=15).returncode == 0 and clean
            except (OSError, subprocess.TimeoutExpired):
                clean = False
        if owned["built"]:
            try:
                clean = run(["docker", "image", "rm", owned["image"]], timeout=15).returncode == 0 and clean
            except (OSError, subprocess.TimeoutExpired):
                clean = False
        receipt["cleanup_complete"] = clean
        receipt["owned_containers"] = owned["containers"]
        if not clean:
            receipt["outcome"] = "failed"
        Path(options.report).write_text(json.dumps(receipt, indent=2) + "\n")
    return 0 if receipt["outcome"] == "source_free_image_subset_pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
