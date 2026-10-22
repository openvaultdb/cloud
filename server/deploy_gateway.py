#!/usr/bin/env python3
"""Reviewed Go gateway rollout controller. Runs only from explicit deployment workflow.
Origin/proxy secret values are never fetched: pinned references go to Cloud Run.
First-bootstrap CAS uses an existing ephemeral OAuth credential only in memory.
"""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import time

SHA = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
BINDING = re.compile(r"[a-zA-Z0-9_-]+:[1-9][0-9]*")


def cloud(*args):
    return json.loads(subprocess.check_output(["gcloud", *args, "--project="+os.environ["PROJECT_ID"], "--region="+os.environ["REGION"], "--format=json"], text=True))


def require_current_main(sha):
    output = subprocess.check_output(["git", "ls-remote", "origin", "refs/heads/main"], text=True)
    fields = output.split()
    if fields != [sha, "refs/heads/main"]:
        raise RuntimeError("candidate is no longer authoritative main")


def identity(name):
    revision = cloud("run", "revisions", "describe", name)
    status = revision.get("status", {})
    digest = status.get("imageDigest", "")
    sha = revision.get("metadata", {}).get("labels", {}).get("source-sha", "")
    if "@sha256:" not in digest or not SHA.fullmatch(sha):
        raise RuntimeError("revision lacks immutable image/source identity")
    return {"revision": name, "image": digest, "sourceSha": sha}


def check_probes(container, require_startup):
    # Liveness/readiness HTTP or gRPC probes cannot satisfy the mandatory edge
    # credential. Do not silently remove an operator's health policy.
    if container.get("livenessProbe") or container.get("readinessProbe"):
        raise RuntimeError("unsupported inherited liveness/readiness probe; review before deployment")
    if require_startup:
        probe = container.get("startupProbe", {})
        if probe.get("tcpSocket") != {"port": 8080} or "httpGet" in probe or "grpc" in probe:
            raise RuntimeError("revision requires TCP startup on serving port8080")
        ports = container.get("ports", [])
        if len(ports) != 1 or ports[0].get("containerPort") != 8080 or ports[0].get("name", "http1") != "http1":
            raise RuntimeError("revision serving port differs from pinned TCP probe")


def single_container(spec):
    containers = spec.get("containers", [])
    if len(containers) != 1:
        raise RuntimeError("exactly one gateway container required")
    return containers[0]


def retained_origin(proof, service):
    from urllib.parse import urlsplit
    value = os.environ["OVDB_DEFAULT_ORIGIN_URL"]
    url = urlsplit(value)
    if url.scheme != "https" or not url.hostname or not re.fullmatch(r"[a-z0-9-]+(?:\.[a-z0-9-]+)*\.run\.app", url.hostname) or url.netloc != url.hostname or url.path or url.query or url.fragment or value != "https://"+url.hostname:
        raise RuntimeError("invalid retained default origin URL")
    expected = {"project": os.environ["PROJECT_ID"], "region": os.environ["REGION"], "service": service, "defaultUrl": value}
    if proof.get("originIdentity") != expected:
        raise RuntimeError("accepted rollback receipt does not bind retained service origin")
    return value


def check_service_origin(service, retained):
    current = service.get("status", {}).get("url")
    if current and current != retained:
        raise RuntimeError("service default origin differs from retained accepted identity")


def secure_revision(name, origin_binding):
    revision = cloud("run", "revisions", "describe", name)
    container = single_container(revision["spec"])
    check_probes(container, True)
    env = {item["name"]: item for item in container.get("env", [])}
    wanted_name, wanted_version = origin_binding.split(":")
    actual = env.get("OVDB_ORIGIN_SECRET", {}).get("valueFrom", {}).get("secretKeyRef", {})
    if actual.get("name") != wanted_name or actual.get("key") != wanted_version:
        raise RuntimeError("rollback revision does not carry the selected pinned origin credential")
    return identity(name)


def traffic(service):
    return sorted([{"revisionName": t["revisionName"], "percent": t["percent"]} for t in service["status"].get("traffic", []) if t.get("percent", 0)], key=lambda t: t["revisionName"])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("accepted-rollback", "first-bootstrap"), default="accepted-rollback")
    parser.add_argument("--bootstrap-action", choices=("capture", "quiesce", "protect", "open-for-mapping", "status", "seal-after-acceptance"))
    parser.add_argument("--bootstrap-state", default="ovdb-bootstrap-state.json")
    parser.add_argument("--bootstrap-ingress-inventory")
    parser.add_argument("--bootstrap-functional-acceptance")
    parser.add_argument("--phase", choices=("protect-origin", "mapped"))
    args = parser.parse_args()
    if args.mode == "first-bootstrap":
        if not args.bootstrap_action:
            parser.error("first-bootstrap requires explicit action")
        from bootstrap_gateway import main as bootstrap_main
        return bootstrap_main(args.bootstrap_action, args.bootstrap_state, args.bootstrap_ingress_inventory, args.bootstrap_functional_acceptance)
    if not args.phase or args.bootstrap_action:
        parser.error("ordinary mode requires phase and cannot consume bootstrap actions")
    if Path(args.bootstrap_state).exists():
        from bootstrap_gateway import load as load_bootstrap
        bootstrap_state = load_bootstrap(Path(args.bootstrap_state))
        if bootstrap_state.get("functionalRollbackAccepted") is not True or bootstrap_state.get("stage") != "mapped-accepted-default-disabled" or bootstrap_state.get("unresolved"):
            raise RuntimeError("first bootstrap pending; ordinary rollback mode forbidden")
    sha = os.environ["DEPLOY_SHA"]
    origin_binding = os.environ["OVDB_ORIGIN_SECRET_BINDING"]
    proxy_binding = os.environ["OVDB_DEVICE_AUTH_PROXY_SECRET_BINDING"]
    sneat = os.environ["SNEAT_API_ORIGIN"]
    if not SHA.fullmatch(sha) or not BINDING.fullmatch(origin_binding) or not BINDING.fullmatch(proxy_binding):
        raise RuntimeError("exact source and pinned secret resource versions required")
    from urllib.parse import urlsplit
    parsed = urlsplit(sneat)
    if parsed.scheme != "https" or not parsed.hostname or parsed.netloc != parsed.hostname or parsed.path or parsed.query or parsed.fragment:
        raise RuntimeError("invalid fixed Sneat API origin")
    service = os.environ["SERVICE_NAME"]
    rollback = secure_revision(os.environ["OVDB_SECURE_ROLLBACK_REVISION"], origin_binding)
    # This external receipt must be collected by the future acceptance stage,
    # not fabricated from an environment-variable name or traffic percentage.
    proof = json.loads(Path("secure-rollback-acceptance.json").read_text())
    if proof.get("identity") != rollback or proof.get("originRejected") is not True or proof.get("routesAccepted") is not True:
        raise RuntimeError("independently accepted secure rollback receipt required")
    default_origin = retained_origin(proof, service)
    before = cloud("run", "services", "describe", service)
    check_service_origin(before, default_origin)
    check_probes(single_container(before["spec"]["template"]["spec"]), False)
    digest = subprocess.check_output(["gcloud", "artifacts", "docker", "images", "describe", os.environ["IMAGE"], "--format=value(image_summary.digest)"], text=True).strip()
    if not DIGEST.fullmatch(digest):
        raise RuntimeError("invalid candidate digest")
    image = os.environ["IMAGE"].rsplit(":", 1)[0]+"@"+digest
    receipt = {"mergeSha": sha, "candidateImage": image, "phase": "captured", "priorTraffic": traffic(before), "secureRollback": rollback, "publicOrigins": ["https://cloud.openvaultdb.com", "https://ovdb.demodb.dev"], "startedAt": int(time.time())}
    receipt_path = Path("shipping-publication-deploy-receipt.json")
    def save(phase):
        receipt["phase"] = phase
        receipt_path.write_text(json.dumps(receipt, indent=2, sort_keys=True)+"\n")
    require_current_main(sha)
    try:
        save("deploy-started")
        command = ["run", "deploy", service, "--image="+image, "--labels=source-sha="+sha, "--no-traffic", "--port=8080", "--startup-probe=tcpSocket.port=8080,periodSeconds=10,timeoutSeconds=1,failureThreshold=24", "--allow-unauthenticated", "--ingress=all", "--service-account=chinook-runtime@openvaultdb.iam.gserviceaccount.com", "--min-instances=0", "--max-instances=2", "--memory=512Mi", "--concurrency=2", "--cpu=1", "--timeout=120", "--update-secrets=OVDB_ORIGIN_SECRET="+origin_binding+",OVDB_DEVICE_AUTH_PROXY_SECRET="+proxy_binding, "--update-env-vars=SNEAT_API_ORIGIN="+sneat+",OVDB_ORIGIN_SECRET_HEADER=X-OVDB-Origin-Secret,OVDB_ECB_ENABLED=false,OVDB_IANA_ENABLED=false", "--quiet"]
        if args.phase == "mapped":
            command.append("--no-default-url")
        created = cloud(*command)
        name = created.get("status", {}).get("latestCreatedRevisionName", "")
        if not re.fullmatch(r"[a-z][a-z0-9-]{0,62}", name):
            raise RuntimeError("missing created revision")
        candidate = secure_revision(name, origin_binding)
        if candidate["image"] != image or candidate["sourceSha"] != sha:
            raise RuntimeError("created image/source differs from captured candidate")
        require_current_main(sha)
        receipt["createdIdentity"] = candidate
        save("route-created-revision")
        cloud("run", "services", "update-traffic", service, "--to-revisions="+name+"=100", "--clear-tags", "--quiet")
        current = cloud("run", "services", "describe", service)
        if traffic(current) != [{"revisionName": name, "percent": 100}] or identity(name) != candidate:
            raise RuntimeError("serving identity differs from named candidate")
        receipt.update(deployedRevision=name, deployedImage=image, deployedSourceSha=sha, deployedTraffic=traffic(current))
        save("verify-origin-boundary")
        probe = ["python3", "gateway_probe.py", "--phase", args.phase, "--receipt", "gateway-live-receipt.json"]
        check_service_origin(current, default_origin)
        probe += ["--default-url", default_origin]
        subprocess.run(probe, check=True)
        if args.phase == "mapped":
            receipt["originBoundaryReceipt"] = "gateway-live-receipt.json"
            save("verify-public-journeys")
            subprocess.run(["python3", "smoke_inventory.py", "--origin", "https://cloud.openvaultdb.com", "--inventory", "fixture/output/inventory.json", "--publication-receipt", str(receipt_path), "--receipt", "shipping-publication-live-receipt.json"], check=True)
            if traffic(cloud("run", "services", "describe", service)) != receipt["deployedTraffic"] or identity(name) != candidate:
                raise RuntimeError("identity changed during live acceptance")
        receipt["authenticatedDeviceJourneyPending"] = True
        save("public-data-accepted-auth-journey-pending" if args.phase == "mapped" else "origin-protected-public-acceptance-pending")
    except Exception:
        save("rolling-back")
        # Never restore prior naked public traffic. Only the independently
        # accepted immutable secure revision is eligible for rollback.
        cloud("run", "services", "update-traffic", service, "--to-revisions="+rollback["revision"]+"=100", "--clear-tags", "--quiet")
        restored = cloud("run", "services", "describe", service)
        good = traffic(restored) == [{"revisionName": rollback["revision"], "percent": 100}] and secure_revision(rollback["revision"], origin_binding) == rollback
        live = False
        if good:
            try:
                rollback_probe = ["python3", "gateway_probe.py", "--phase", args.phase, "--receipt", "gateway-rollback-live-receipt.json"]
                check_service_origin(restored, default_origin)
                rollback_probe += ["--default-url", default_origin]
                subprocess.run(rollback_probe, check=True)
                if args.phase == "mapped":
                    subprocess.run(["python3", "smoke_inventory.py", "--origin", "https://cloud.openvaultdb.com", "--inventory", "fixture/output/inventory.json", "--receipt", "shipping-publication-rollback-live-receipt.json"], check=True)
                live = traffic(cloud("run", "services", "describe", service)) == [{"revisionName": rollback["revision"], "percent": 100}] and secure_revision(rollback["revision"], origin_binding) == rollback
            except Exception:
                live = False
        Path("shipping-publication-rollback-receipt.json").write_text(json.dumps({"identity": rollback, "immutableTrafficRestored": good, "liveAcceptancePassed": live}, indent=2)+"\n")
        # A successful rollback never turns the failed candidate green.
        raise


if __name__ == "__main__":
    main()
