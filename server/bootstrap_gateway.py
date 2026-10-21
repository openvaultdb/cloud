#!/usr/bin/env python3
"""First bootstrap only. Ordinary accepted-rollback deployment stays separate.

Uses the documented regional v1 replaceService API with the ORIGINAL captured
resourceVersion. gcloud replace would silently substitute a freshly read version.
Existing Google OAuth credentials are used only in memory; no origin/proxy secret
values are fetched. No mapping, DNS, certificate or auth-authority writes exist.
"""
import copy
import hashlib
import http.client
import ipaddress
import socket
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import time
from urllib.parse import urlsplit

import deploy_gateway as deploy
from gateway_probe import direct

HOSTS = ["cloud.openvaultdb.com", "ovdb.demodb.dev"]
PATHS = ["/v1", "/api/devices", "/device/"]
DISABLED = "run.googleapis.com/default-url-disabled"
STATE_VERSION = "ovdb-first-bootstrap/1"


class ProviderFailure(RuntimeError):
    """Fixed redacted stage/status message only, never an HTTP response body."""


class Conflict(RuntimeError):
    """A conditional write lost its fence. Never refresh-and-overwrite."""


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def target():
    # This controller is for the reviewed existing service, not a service factory.
    expected = ("openvaultdb", "europe-west1", "chinook-ovdb")
    if tuple(os.environ[key] for key in ("PROJECT_ID", "REGION", "SERVICE_NAME")) != expected:
        raise RuntimeError("bootstrap target differs from reviewed service")
    return dict(zip(("project", "region", "service"), expected))


def replace_service(before, desired):
    """One conditional PUT, no retries/redirects; never persist its full body."""
    scope = target()
    prior = before["metadata"]
    if any(desired["metadata"].get(key) != prior.get(key) for key in ("uid", "name", "namespace", "resourceVersion")):
        raise Conflict("bootstrap replacement identity/version changed")
    if not prior.get("resourceVersion") or prior.get("name") != scope["service"] or not re.fullmatch(r"[a-z0-9-]+", str(prior.get("namespace", ""))):
        raise Conflict("bootstrap missing exact service version/namespace")
    host = scope["region"]+"-run.googleapis.com"
    path = "/apis/serving.knative.dev/v1/namespaces/"+str(prior["namespace"])+"/services/"+scope["service"]
    token = ""
    connection = None
    try:
        auth = subprocess.run(["gcloud", "auth", "print-access-token"], capture_output=True, text=True, timeout=30)
        token = auth.stdout.strip() if auth.returncode == 0 else ""
        if not token or len(token) > 8192 or any(c.isspace() for c in token):
            raise RuntimeError("bootstrap CAS authentication failed")
        body = copy.deepcopy(desired)
        body.pop("status", None)
        connection = http.client.HTTPSConnection(host, timeout=30)
        connection.request("PUT", path, body=json.dumps(body).encode(), headers={"Authorization": "Bearer "+token, "Content-Type": "application/json"})
        response = connection.getresponse()
        if response.status == 409:
            raise Conflict("bootstrap CAS HTTP409; no retry")
        if response.status != 200:
            raise ProviderFailure("bootstrap CAS HTTP"+str(response.status))
        raw = response.read(2*1024*1024+1)
        if len(raw) > 2*1024*1024:
            raise RuntimeError("bootstrap CAS response capacity")
        value = json.loads(raw)
        if not isinstance(value, dict):
            raise RuntimeError("bootstrap CAS invalid service response")
        if any(value.get("metadata", {}).get(key) != prior.get(key) for key in ("uid", "name", "namespace")):
            raise Conflict("bootstrap CAS returned different service identity")
        return value
    except (Conflict, RuntimeError):
        raise
    except Exception:
        raise RuntimeError("bootstrap CAS transport/auth/response failed") from None
    finally:
        token = ""
        if connection:
            connection.close()


def disabled(service):
    value = service.get("metadata", {}).get("annotations", {}).get(DISABLED, "false")
    if value not in ("true", "false"):
        raise RuntimeError("ambiguous default URL state")
    return value == "true"


def tags(service):
    return [row for section in (service.get("spec", {}), service.get("status", {})) for row in section.get("traffic", []) if row.get("tag")]


def config(service):
    spec = copy.deepcopy(service["spec"])
    # These documented no-op defaults may be materialized by the API. Unknown
    # non-default config remains in the digest and cannot silently disappear.
    for row in spec.get("traffic", []):
        if row.get("latestRevision") is False:
            row.pop("latestRevision")
    template = spec.get("template", {})
    if template.get("metadata", {}).get("creationTimestamp", "absent") is None:
        template["metadata"].pop("creationTimestamp")
    for container in template.get("spec", {}).get("containers", []):
        for port in container.get("ports", []):
            if port.get("name") == "http1":
                port.pop("name")
        for name in ("startupProbe", "livenessProbe", "readinessProbe"):
            probe = container.get(name, {})
            if probe.get("initialDelaySeconds") == 0:
                probe.pop("initialDelaySeconds")
            if probe.get("successThreshold") == 1:
                probe.pop("successThreshold")
    return {"spec": spec, "defaultDisabled": disabled(service)}


def generation(value):
    if isinstance(value,bool) or not re.fullmatch(r"[1-9][0-9]*",str(value)):
        raise Conflict("invalid generation identity")
    return int(value)


def fence(service):
    scope = target()
    meta = service.get("metadata", {})
    current_generation = generation(meta.get("generation"))
    if meta.get("name") != scope["service"] or not meta.get("uid") or not meta.get("resourceVersion") or not meta.get("namespace") or not isinstance(meta.get("resourceVersion"), str):
        raise Conflict("bootstrap service identity/generation unavailable")
    return {"uid": meta["uid"], "namespace": str(meta["namespace"]), "generation": current_generation, "resourceVersion": meta["resourceVersion"], "configSHA256": digest(config(service))}


def ready(service):
    return generation(service.get("status", {}).get("observedGeneration")) == generation(service["metadata"]["generation"]) and any(c.get("type") == "Ready" and c.get("status") == "True" for c in service.get("status", {}).get("conditions", []))


def endpoints(service):
    values = [service.get("status", {}).get("url"), service.get("status", {}).get("address", {}).get("url")]
    for section in (service.get("spec", {}), service.get("status", {})):
        values += [row.get("url") for row in section.get("traffic", [])]
    annotation = service.get("metadata", {}).get("annotations", {}).get("run.googleapis.com/urls")
    if annotation:
        decoded = json.loads(annotation)
        if not isinstance(decoded, list):
            raise RuntimeError("invalid service alias inventory")
        values += decoded
    result = []
    for value in values:
        if not value:
            continue
        url = urlsplit(value)
        if url.scheme != "https" or not url.hostname or not url.hostname.endswith(".run.app") or url.netloc != url.hostname or url.path or url.query or url.fragment or value != "https://"+url.hostname:
            raise RuntimeError("unexpected ingress identity; operator inventory required")
        result.append(value)
    return sorted(set(result))


def record(path, state):
    # Receipts contain identities/digests only, never complete service env/config.
    if path.name != str(path) or path.is_symlink():
        raise RuntimeError("bootstrap state must be an owned local filename")
    with tempfile.NamedTemporaryFile(mode="w", dir=".", prefix=".bootstrap-", delete=False) as stream:
        scratch = Path(stream.name)
        try:
            os.chmod(scratch, 0o600)
            json.dump(state, stream, sort_keys=True, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
            os.replace(scratch, path)
            directory = os.open(".", os.O_RDONLY | os.O_DIRECTORY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            scratch.unlink(missing_ok=True)


def load(path):
    details = path.lstat()
    if not stat.S_ISREG(details.st_mode) or details.st_nlink != 1 or details.st_uid != os.getuid() or details.st_mode & 0o022:
        raise RuntimeError("bootstrap state is not an owned regular file")
    state = json.loads(path.read_text())
    if state.get("contract") != STATE_VERSION or state.get("mode") != "first-bootstrap" or state.get("target") != target():
        raise RuntimeError("bootstrap receipt mode/target mismatch")
    return state


class Bootstrap:
    def __init__(self, path):
        self.path = Path(path)
        self.state = load(self.path) if self.path.exists() else None

    def save(self, stage):
        self.state["stage"] = stage
        self.state["updatedAt"] = int(time.time())
        record(self.path, self.state)

    def read(self):
        service = deploy.cloud("run", "services", "describe", target()["service"])
        current = fence(service)
        if self.state and current["uid"] != self.state["fence"]["uid"]:
            raise Conflict("bootstrap UID changed; refuse replacement by name")
        return service

    def mappings(self):
        value = deploy.cloud("run", "domain-mappings", "list", "--platform=managed")
        if not isinstance(value, list):
            raise RuntimeError("incomplete mapping list; operator inventory required")
        return value

    def absence(self):
        for mapping in self.mappings():
            if mapping.get("metadata", {}).get("name") in HOSTS or mapping.get("spec", {}).get("routeName") == target()["service"]:
                raise RuntimeError("bootstrap requires mappings absent")

    def capture(self, inventory_path):
        if self.state:
            raise RuntimeError("bootstrap already captured; do not overwrite state")
        service = self.read()
        if not ready(service):
            raise RuntimeError("baseline not reconciled")
        current = fence(service)
        self.absence()
        # Root's separately collected complete bounded inventory is mandatory;
        # a successful two-name/service-spec lookup cannot prove no LB/other ingress.
        deploy.require_current_main(os.environ["DEPLOY_SHA"])
        inventory = json.loads(Path(inventory_path).read_text())
        if inventory.get("target") != target() or inventory.get("fence") != current or inventory.get("complete") is not True or inventory.get("mappingResources") != [] or inventory.get("otherPublicIngress") != [] or inventory.get("contract") != "ovdb-bootstrap-ingress-inventory/1" or inventory.get("coverage") != ["project-region-mappings", "service-direct-alias-tags", "bounded-service-public-ingress"] or not isinstance(inventory.get("provenance"), list) or not inventory["provenance"] or any(not isinstance(item,dict) or not item.get("source") or item.get("complete") is not True or not re.fullmatch(r"[0-9a-f]{64}", str(item.get("resultSHA256", ""))) for item in inventory["provenance"]) or not isinstance(inventory.get("observedAt"), int) or not 0 <= time.time()-inventory["observedAt"] <= 300:
            raise RuntimeError("fresh complete attributable root ingress inventory required")
        urls = sorted(set(endpoints(service)+inventory.get("directGoogleURLs", [])))
        # Apply the same validation to external aliases, not just gcloud fields.
        if not urls or endpoints({"status":{"traffic":[{"url":v} for v in urls]}}) != urls:
            raise RuntimeError("invalid or missing actual origin identities")
        if fence(self.read()) != current:
            raise Conflict("capture generation/config changed")
        tag_urls = sorted(set([row["url"] for section in (service["spec"],service["status"]) for row in section.get("traffic",[]) if row.get("tag") and row.get("url")]+inventory.get("tagGoogleURLs", [])))
        if not set(tag_urls).issubset(urls):
            raise RuntimeError("tag identities absent from captured direct inventory")
        self.state = {"tagGoogleURLs":tag_urls,"contract": STATE_VERSION, "mode": "first-bootstrap", "target": target(), "fence": current, "capturedFence": current, "directGoogleURLs": urls, "inventorySHA256": digest(inventory), "priorTraffic": deploy.traffic(service), "routesAccepted": False, "functionalRollbackAccepted": False, "quiescenceStarted": False}
        self.save("captured")

    def current(self):
        service = self.read()
        if self.state.get("pending"):
            pending = self.state["pending"]
            if self.state.get("unresolved"):
                raise Conflict("unresolved conditional mutation; root remediation required")
            if pending.get("responseGeneration") is None or generation(service["metadata"]["generation"]) != pending["responseGeneration"] or digest(config(service)) != pending["configSHA256"] or not ready(service):
                raise Conflict("pending mutation not attributable/reconciled; root remediation required")
            self.state["fence"] = fence(service)
            self.state.pop("pending")
            self.save(self.state["stage"])
        elif fence(service) != self.state["fence"]:
            raise Conflict("bootstrap version/generation/config drift")
        if not ready(service):
            raise RuntimeError("service reconciliation pending")
        return service

    def mutate(self, desired):
        before = self.current()
        if desired["metadata"] != before["metadata"]:
            # Only annotations may change; identity/version must remain exact.
            for key in ("uid", "name", "namespace", "resourceVersion", "generation"):
                if desired["metadata"].get(key) != before["metadata"].get(key):
                    raise Conflict("mutation identity differs from captured fence")
        self.state["pending"] = {"configSHA256": digest(config(desired)), "resourceVersion": before["metadata"]["resourceVersion"]}
        self.save(self.state["stage"])
        response = replace_service(before, desired)
        response_fence = fence(response)
        if response_fence["uid"] != self.state["fence"]["uid"] or response_fence["generation"] < self.state["fence"]["generation"] or digest(config(response)) != self.state["pending"]["configSHA256"]:
            raise Conflict("conditional response does not attribute expected service/config")
        self.state["pending"]["responseGeneration"] = response_fence["generation"]
        self.state["pending"]["responseVersion"] = response_fence["resourceVersion"]
        self.save(self.state["stage"])
        # Read actual state; CLI/HTTP return status alone cannot advance.
        for _ in range(5):
            value = self.read()
            if generation(value["metadata"]["generation"]) != self.state["pending"]["responseGeneration"]:
                raise Conflict("generation drift after conditional response")
            if digest(config(value)) == self.state["pending"]["configSHA256"] and ready(value):
                self.state["fence"] = fence(value)
                self.state.pop("pending")
                self.save(self.state["stage"])
                return value
            time.sleep(1)
        raise RuntimeError("bootstrap mutation reconciliation pending")

    def closed(self, service):
        if not disabled(service) or tags(service):
            raise RuntimeError("default/tag ingress not closed")
        observations = []
        for value in self.state["directGoogleURLs"]:
            host = urlsplit(value).hostname
            for path in PATHS:
                observation = direct(host, host, path)
                if observation.get("host") != host or observation.get("path") != path or observation.get("status") not in (403, 404) or not observation.get("certificateSHA256"):
                    raise RuntimeError("attributable known-path origin rejection required")
                observations.append(observation)
        self.state["closureObservations"] = observations

    def close(self):
        value = copy.deepcopy(self.current())
        value.setdefault("metadata", {}).setdefault("annotations", {})[DISABLED] = "true"
        for row in value["spec"].get("traffic", []):
            row.pop("tag", None)
            row.pop("url", None)
        service = self.mutate(value)
        self.closed(service)
        mappings = [row for row in self.mappings() if row.get("spec",{}).get("routeName")==target()["service"] or row.get("metadata",{}).get("name") in HOSTS]
        if mappings:
            self.protected(service)
            for mapping in mappings:
                host = mapping.get("metadata",{}).get("name")
                if host not in HOSTS or mapping.get("spec",{}).get("routeName") != target()["service"]:
                    raise RuntimeError("unexpected mapped ingress requires root remediation")
                addresses = set()
                for row in mapping.get("status",{}).get("resourceRecords",[]):
                    if row.get("type") in ("A","AAAA"):
                        addresses.add(str(ipaddress.ip_address(row["rrdata"])))
                    elif row.get("type")=="CNAME":
                        name=row.get("rrdata","").rstrip(".")
                        if not re.fullmatch(r"[A-Za-z0-9.-]+",name):
                            raise RuntimeError("invalid Google mapping target")
                        addresses.update(item[4][0] for item in socket.getaddrinfo(name,443,type=socket.SOCK_STREAM))
                if not addresses:
                    raise RuntimeError("mapped origin target unavailable; closure unresolved")
                for address in sorted(addresses):
                    for path in PATHS:
                        result=direct(host,address,path)
                        if result.get("host")!=host or result.get("path")!=path or result.get("status")!=403 or not result.get("certificateSHA256"):
                            raise RuntimeError("mapped origin protection not observed")
        return service

    def protected(self, service):
        expected = self.state.get("candidate")
        if not expected or service["spec"]["template"]["metadata"].get("name") != expected["revision"] or deploy.traffic(service) != [{"revisionName": expected["revision"], "percent": 100}] or tags(service) or deploy.secure_revision(expected["revision"], os.environ["OVDB_ORIGIN_SECRET_BINDING"]) != expected:
            raise RuntimeError("exact protected serving/config identity required")

        self.runtime_revision(expected["revision"])

    def runtime_revision(self, name):
        revision = deploy.cloud("run", "revisions", "describe", name)
        if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in revision.get("status",{}).get("conditions",[])):
            raise RuntimeError("protected revision startup not ready")
        env = {row["name"]:row for row in deploy.single_container(revision["spec"]).get("env", [])}
        for key, binding in (("OVDB_ORIGIN_SECRET",self.state["deployment"]["originBinding"]),("OVDB_DEVICE_AUTH_PROXY_SECRET",self.state["deployment"]["proxyBinding"])):
            secret_name, version = binding.split(":")
            if env.get(key,{}).get("valueFrom",{}).get("secretKeyRef") != {"name":secret_name,"key":version}:
                raise RuntimeError("protected revision credential binding differs from pinned intent")
        for key, value in (("SNEAT_API_ORIGIN",self.state["deployment"]["sneatOrigin"]),("OVDB_ORIGIN_SECRET_HEADER","X-OVDB-Origin-Secret"),("OVDB_ECB_ENABLED","false"),("OVDB_IANA_ENABLED","false")):
            if env.get(key,{}).get("value") != value:
                raise RuntimeError("protected revision host configuration differs from pinned intent")

    def quiesce(self):
        if self.state["stage"] not in ("captured", "quiescing"):
            raise RuntimeError("quiescence requires captured bootstrap")
        if self.state["stage"] == "quiescing":
            service = self.current()
            self.absence()
            self.closed(service)
            self.save("quiesced")
            return
        self.absence()
        deploy.require_current_main(os.environ["DEPLOY_SHA"])
        self.state["quiescenceStarted"] = True
        self.save("quiescing")
        self.close()
        self.save("quiesced")

    def protect(self):
        if self.state["stage"] not in ("quiesced", "quiesced-after-failure", "protected-deploying"):
            raise RuntimeError("protected deployment requires quiescence")
        service = self.current()
        self.absence()
        self.closed(service)
        sha = os.environ["DEPLOY_SHA"]
        image = os.environ["BOOTSTRAP_IMAGE_DIGEST"]
        origin = os.environ["OVDB_ORIGIN_SECRET_BINDING"]
        proxy = os.environ["OVDB_DEVICE_AUTH_PROXY_SECRET_BINDING"]
        if not deploy.SHA.fullmatch(sha) or not re.fullmatch(r"europe-west1-docker\.pkg\.dev/openvaultdb/chinook-ovdb/server@sha256:[0-9a-f]{64}", image) or not deploy.BINDING.fullmatch(origin) or not deploy.BINDING.fullmatch(proxy):
            raise RuntimeError("reviewed exact source/image and pinned credentials required")
        sneat = os.environ["SNEAT_API_ORIGIN"]
        parsed = urlsplit(sneat)
        if parsed.scheme != "https" or not parsed.hostname or parsed.netloc != parsed.hostname or parsed.path or parsed.query or parsed.fragment or sneat != "https://"+parsed.hostname:
            raise RuntimeError("invalid fixed existing Sneat origin")
        deployment = {"sourceSha":sha,"image":image,"originBinding":origin,"proxyBinding":proxy,"sneatOrigin":sneat}
        if self.state.get("deployment") not in (None,deployment):
            raise Conflict("bootstrap deployment intent changed across resume")
        self.state["deployment"] = deployment
        deploy.require_current_main(sha)
        self.save("protected-deploying")
        value = copy.deepcopy(service)
        revision = target()["service"]+"-bootstrap-"+sha[:12]
        template = value["spec"]["template"]
        template.setdefault("metadata", {})["name"] = revision
        template["metadata"].setdefault("labels", {})["source-sha"] = sha
        container = deploy.single_container(template["spec"])
        deploy.check_probes(container, False)
        limits = container.get("resources", {}).get("limits", {})
        if limits.get("memory") != "512Mi" or limits.get("cpu") not in ("1", "1000m") or template["spec"].get("containerConcurrency") != 2 or template["spec"].get("timeoutSeconds") != 120 or template["spec"].get("serviceAccountName") != "chinook-runtime@openvaultdb.iam.gserviceaccount.com":
            raise RuntimeError("bootstrap baseline resources differ from reviewed limits")
        revision_annotations = template["metadata"].get("annotations", {})
        if revision_annotations.get("autoscaling.knative.dev/maxScale") != "2" and value["metadata"].get("annotations", {}).get("run.googleapis.com/maxScale") != "2":
            raise RuntimeError("bootstrap requires existing reviewed max-instance bound")
        container.update(image=image, ports=[{"containerPort":8080}], startupProbe={"tcpSocket":{"port":8080},"periodSeconds":10,"timeoutSeconds":1,"failureThreshold":24})
        env = {item["name"]: item for item in container.get("env", [])}
        for key, binding in (("OVDB_ORIGIN_SECRET",origin),("OVDB_DEVICE_AUTH_PROXY_SECRET",proxy)):
            name, version = binding.split(":")
            env[key] = {"name":key,"valueFrom":{"secretKeyRef":{"name":name,"key":version}}}
        for key, text in (("SNEAT_API_ORIGIN",sneat),("OVDB_ORIGIN_SECRET_HEADER","X-OVDB-Origin-Secret"),("OVDB_ECB_ENABLED","false"),("OVDB_IANA_ENABLED","false")):
            env[key] = {"name":key,"value":text}
        container["env"] = list(env.values())
        # Preserve prior named percentages while creating the protected revision.
        value["spec"]["traffic"] = copy.deepcopy(deploy.traffic(service))
        self.mutate(value)
        candidate = deploy.secure_revision(revision, origin)
        if candidate != {"revision":revision,"image":image,"sourceSha":sha}:
            raise RuntimeError("created bootstrap candidate image/source mismatch")
        self.runtime_revision(revision)
        self.state["candidate"] = candidate
        self.save("protected-deploying")
        deploy.require_current_main(sha)
        value = copy.deepcopy(self.current())
        value["spec"]["traffic"] = [{"revisionName":revision,"percent":100}]
        service = self.mutate(value)
        self.protected(service)
        self.closed(service)
        self.save("protected-serving-default-disabled")

    def open_for_mapping(self):
        if self.state["stage"] != "protected-serving-default-disabled":
            raise RuntimeError("protected verification must precede default opening")
        service = self.current()
        self.protected(service)
        self.closed(service)
        self.absence()
        deploy.require_current_main(self.state["deployment"]["sourceSha"])
        value = copy.deepcopy(service)
        value["metadata"].setdefault("annotations", {})[DISABLED] = "false"
        service = self.mutate(value)
        self.protected(service)
        # Invalid credential variants need no secret read. This opening never
        # creates a functional routesAccepted/rollback receipt.
        self.closed_admission(service)
        self.save("protected-default-enabled-for-mapping")

    def closed_admission(self, service):
        self.protected(service)
        if disabled(service):
            raise RuntimeError("protected default was not explicitly enabled")
        for value in self.state["directGoogleURLs"]:
            host = urlsplit(value).hostname
            for path in PATHS:
                for variant in ("missing", "wrong", "duplicate"):
                    result = direct(host, host, path, credential_variant=variant)
                    allowed = (403,404) if value in self.state["tagGoogleURLs"] else (403,)
                    if result.get("status") not in allowed or result.get("host") != host or result.get("path") != path or result.get("credentialVariant") != variant or not result.get("certificateSHA256"):
                        raise RuntimeError("protected default credential rejection required")

    def seal(self, acceptance_path):
        if self.state["stage"] not in ("protected-default-enabled-for-mapping", "mapped-accepted"):
            raise RuntimeError("mapping acceptance requires verified protected opening")
        service = self.current()
        if disabled(service):
            self.protected(service)
            self.closed(service)
        else:
            self.closed_admission(service)
        mappings = [m for m in self.mappings() if m.get("metadata", {}).get("name") in HOSTS]
        if sorted(m["metadata"]["name"] for m in mappings) != sorted(HOSTS):
            raise RuntimeError("both attributable mapping resources required")
        for mapping in mappings:
            if not mapping.get("metadata",{}).get("uid") or generation(mapping.get("metadata",{}).get("generation")) != generation(mapping.get("status",{}).get("observedGeneration")):
                raise RuntimeError("mapping readiness generation not attributable")
            if mapping.get("spec", {}).get("routeName") != target()["service"] or not mapping.get("status", {}).get("resourceRecords") or not any(c.get("type")=="Ready" and c.get("status")=="True" for c in mapping.get("status", {}).get("conditions", [])):
                raise RuntimeError("mapping DNS/certificate readiness not observed")
        proof = json.loads(Path(acceptance_path).read_text())
        if proof.get("contract") != "ovdb-functional-mapped-acceptance/1" or proof.get("serviceUid") != self.state["fence"]["uid"] or proof.get("identity") != self.state["candidate"] or proof.get("publicHosts") != HOSTS or proof.get("originRejected") is not True or proof.get("routesAccepted") is not True or proof.get("authenticatedJourneyAccepted") is not True or proof.get("completeIngressInventory") is not True or not proof.get("provenance") or not isinstance(proof.get("observedAt"), int) or not 0 <= time.time()-proof["observedAt"] <= 300:
            raise RuntimeError("fresh attributable root functional acceptance required")
        evidence = proof.get("evidenceSHA256", {})
        if set(evidence) != {"dns", "originCertificates", "edgeRules", "dataPinsStreaming", "authenticatedDeviceDatabase"} or any(not isinstance(v,str) or not re.fullmatch(r"[0-9a-f]{64}",v) for v in evidence.values()):
            raise RuntimeError("full separate external evidence digests required")
        deploy.retained_origin(proof, target()["service"])
        # Root's actual authenticated journey is external; repeat independent
        # current origin/public-data probes before final default retirement.
        subprocess.run(["python3", "gateway_probe.py", "--phase", "mapped", "--default-url", os.environ["OVDB_DEFAULT_ORIGIN_URL"], "--receipt", "bootstrap-mapped-origin-receipt.json"], check=True, timeout=180)
        inventory = json.loads(Path("fixture/output/inventory.json").read_text())
        if {item["id"] for item in inventory.get("databases", [])} != {"adventureworks","employees","chinook","northwind","pubs","sakila","shipping","shipping-logs"}:
            raise RuntimeError("all eight native providers required before acceptance")
        subprocess.run(["python3", "smoke_inventory.py", "--origin", "https://cloud.openvaultdb.com", "--inventory", "fixture/output/inventory.json", "--receipt", "bootstrap-mapped-data-receipt.json"], check=True, timeout=300)
        self.protected(self.current())
        self.state["functionalAcceptanceReceiptSHA256"] = digest(proof)
        self.save("mapped-accepted")
        service = self.close()
        self.protected(service)
        self.state["functionalRollbackAccepted"] = True
        # This state is NOT the ordinary identity/routesAccepted receipt. Root
        # retains the separately observed functional evidence as its authority.
        self.save("mapped-accepted-default-disabled")

    def run(self, action, inventory_path=None, acceptance_path=None):
        try:
            if action == "capture":
                return self.capture(inventory_path)
            if not self.state:
                raise RuntimeError("capture required")
            if self.state.get("unresolved"):
                raise Conflict("unresolved bootstrap requires root remediation")
            self.current()
            if action == "quiesce":
                self.quiesce()
            elif action == "protect":
                self.protect()
            elif action == "open-for-mapping":
                self.open_for_mapping()
            elif action == "seal-after-acceptance":
                self.seal(acceptance_path)
            elif action == "status":
                service = self.current()
                if self.state["stage"] == "protected-default-enabled-for-mapping":
                    self.closed_admission(service)
                    self.state["pendingExternal"] = "root mapping-resource/DNS/certificate and actual both-host data/auth acceptance; no functional rollback yet"
                    self.save(self.state["stage"])
                else:
                    self.closed(service)
            else:
                raise RuntimeError("unsupported bootstrap action")
        except Conflict:
            if self.state:
                self.state["unresolved"] = "service CAS/version/UID conflict; no blind retry or replacement by name"
                self.save(self.state["stage"])
            raise
        except Exception as failure:
            if self.state and isinstance(failure, ProviderFailure):
                self.state["providerFailure"] = str(failure)
            if self.state and self.state.get("quiescenceStarted"):
                try:
                    self.close()
                    self.save("quiesced-after-failure")
                except Exception:
                    self.state["unresolved"] = "post-quiescence closure not verified; root remediation required"
                    self.save(self.state["stage"])
            raise RuntimeError("bootstrap stage failed; see redacted state, never restore naked baseline") from None


def main(action, state_file, inventory_path, acceptance_path):
    # No automatic ordinary-mode fallback and no mapping/DNS/certificate writes.
    # Serialize the one local receipt; provider resourceVersion is the actual CAS.
    import fcntl
    if Path(state_file).name != state_file:
        raise RuntimeError("bootstrap state must be a local filename")
    descriptor = os.open(state_file+".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        Bootstrap(state_file).run(action, inventory_path, acceptance_path)
    finally:
        os.close(descriptor)
