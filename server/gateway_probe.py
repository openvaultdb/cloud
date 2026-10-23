#!/usr/bin/env python3
"""Public/mapped origin acceptance: no credential values are accepted or logged."""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import ssl
from urllib.parse import urlsplit


def direct(host, address, path, credential_variant="missing"):
    connection = http.client.HTTPSConnection(host, timeout=20)
    raw = socket.create_connection((address, 443), timeout=20)
    connection.sock = ssl.create_default_context().wrap_socket(raw, server_hostname=host)
    certificate = connection.sock.getpeercert()
    fingerprint = hashlib.sha256(connection.sock.getpeercert(binary_form=True)).hexdigest()
    try:
        if credential_variant not in ("missing", "wrong", "duplicate"):
            raise RuntimeError("unsupported credential proof variant")
        connection.putrequest("GET", path, skip_host=True)
        connection.putheader("Host", host)
        connection.putheader("CF-Connecting-IP", "192.0.2.1")
        connection.putheader("X-Forwarded-Host", host)
        for _ in range({"missing":0,"wrong":1,"duplicate":2}[credential_variant]):
            connection.putheader("X-OVDB-Origin-Secret", "invalid-bootstrap-proof")
        connection.endheaders()
        response = connection.getresponse()
        response.read(1024)
        return {"host": host, "address": address, "path": path, "status": response.status, "certificateSHA256": fingerprint, "notAfter": certificate.get("notAfter"), "credentialVariant": credential_variant}
    finally:
        connection.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--phase", choices=("protect-origin", "mapped"), required=True)
    parser.add_argument("--default-url")
    parser.add_argument("--receipt", required=True)
    args = parser.parse_args()
    observations = []
    if args.phase == "mapped":
        # Resolve the returned GOOGLE mapping target, never public host DNS (which
        # would merely re-test Cloudflare). This is non-secret deployment config.
        target = os.environ["OVDB_GOOGLE_ORIGIN_TARGET"]
        addresses = sorted({item[4][0] for item in socket.getaddrinfo(target, 443, type=socket.SOCK_STREAM)})
        if not addresses:
            raise RuntimeError("no Google origin address")
        for host in ("cloud.openvaultdb.com", "ovdb.demodb.dev"):
            for path in ("/v1", "/api/devices", "/device/"):
                observation = direct(host, addresses[0], path)
                observations.append(observation)
                if observation["status"] != 403:
                    raise RuntimeError("direct mapped origin bypass")
    if args.default_url:
        parsed = urlsplit(args.default_url)
        if parsed.scheme != "https" or not parsed.hostname or not parsed.hostname.endswith(".run.app") or parsed.netloc != parsed.hostname:
            raise RuntimeError("invalid default service URL")
        observation = direct(parsed.hostname, parsed.hostname, "/v1")
        observations.append(observation)
        if observation["status"] not in (403, 404):
            raise RuntimeError("default origin bypass")
    if not observations:
        raise RuntimeError("no direct origin rejection proof")
    if args.phase == "mapped":
        from smoke_inventory import require_status
        _, body = require_status("https://ovdb.demodb.dev", "/v1")
        if json.loads(body).get("apiUrl") != "https://ovdb.demodb.dev/v1":
            raise RuntimeError("DemoDB branding mismatch")
        for path in ("/device/", "/devices/", "/.well-known/oauth-authorization-server"):
            require_status("https://cloud.openvaultdb.com", path)
        require_status("https://ovdb.demodb.dev", "/api/devices", expected=404)
        for path in ("/ecb-public", "/ecb/v1/databases/ecb/tugql", "/iana/v1/databases/iana-http-status/tugql"):
            require_status("https://cloud.openvaultdb.com", path, expected=503)
    Path(args.receipt).write_text(json.dumps({"phase": args.phase, "originObservations": observations, "automaticRenewalObserved": False, "authenticatedDeviceJourneyPending": True}, indent=2)+"\n")


if __name__ == "__main__":
    main()
