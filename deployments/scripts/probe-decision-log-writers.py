"""DL-1 probe: each decision-log writer's workload certificate may append.

Runs inside the compose network (it needs mtls-management-svc and
governance-decision-log-svc's :8450 listener by name), with the mTLS
bootstrap-token volume mounted read-only at /bootstrap:

  MSYS_NO_PATHCONV=1 docker run --rm --network deployments_default \
    -v deployments_mtls_bootstrap_token:/bootstrap:ro \
    -v "$PWD/scripts:/scripts:ro" python:3.12-slim \
    python -I /scripts/probe-decision-log-writers.py

For every writer it provisions a certificate from mtls-management-svc with
that writer's own bootstrap token (as identity-envelope/mtlsclient does),
then POSTs one decision to https://governance-decision-log-svc:8450 as the
writer's service principal. 201 (or 200 on a rerun) passes. Exit code =
number of failures.
"""
import json
import os
import ssl
import sys
import tempfile
import urllib.error
import urllib.request
import uuid

MTLS = os.environ.get("MTLS_MANAGEMENT_URL", "http://mtls-management-svc:8140")
GDL = os.environ.get("DECISION_LOG_MTLS_URL", "https://governance-decision-log-svc:8450")
TENANT = os.environ.get("TENANT_ID", "11111111-1111-1111-1111-111111111111")
ENTITY = os.environ.get("LEGAL_ENTITY_ID", "22222222-2222-2222-2222-222222222222")
PLATFORM = "00000000-0000-0000-0000-00000000f001"

WRITERS = [
    ("authorization-svc", "svc-authorization", "ACCESS_DL1_PROBE", "DENY"),
    ("jurisdiction-rules-svc", "svc-jurisdiction-rules", "JURISDICTION_DL1_PROBE", "CERTIFIED"),
    ("obligations-svc", "svc-obligations", "OBLIGATION_DL1_PROBE", "RAISED"),
    ("evidence-requirements-svc", "svc-evidence-requirements", "EVIDENCE_DL1_PROBE", "SATISFIED"),
]


def provision(service):
    with open(f"/bootstrap/{service}/token", encoding="utf-8") as f:
        token = f.read().strip()
    body = json.dumps({"legal_entity_id": PLATFORM, "service_name": service,
                       "common_name": service, "rotation_days": 1}).encode()
    req = urllib.request.Request(MTLS + "/v1/mtls/certificates", data=body, method="POST", headers={
        "Content-Type": "application/json", "X-Tenant-Id": PLATFORM, "X-Legal-Entity-Id": PLATFORM,
        "X-Workload-Id": service, "X-Request-Id": uuid.uuid4().hex, "X-Correlation-ID": uuid.uuid4().hex,
        "X-Source-Channel": "system", "X-Purpose-Context": "service_identity_provisioning",
        "Idempotency-Key": uuid.uuid4().hex, "X-Mtls-Bootstrap-Token": token})
    with urllib.request.urlopen(req, timeout=15) as resp:
        out = json.loads(resp.read())
    d = tempfile.mkdtemp()
    paths = {}
    for name, pem in (("cert", out["certificate"]["certificate_pem"]), ("key", out["private_key_pem"]),
                      ("ca", out["ca_certificate_pem"])):
        paths[name] = os.path.join(d, name + ".pem")
        with open(paths[name], "w", encoding="utf-8") as f:
            f.write(pem)
    return paths


def post_decision(paths, principal, action, outcome):
    ctx = ssl.create_default_context(cafile=paths["ca"])
    ctx.load_cert_chain(paths["cert"], paths["key"])
    ctx.check_hostname = True
    decision_id = "dl1-probe-" + principal
    corr = "dl1-probe-" + principal
    body = json.dumps({
        "decision_id": decision_id, "legal_entity_id": ENTITY, "action_type": action, "outcome": outcome,
        "rule_basis": "dl1:probe", "correlation_id": corr,
        "evaluation_context": {"probe": "DL-1 writer wiring"},
    }).encode()
    req = urllib.request.Request(GDL + "/v1/decisions", data=body, method="POST", headers={
        "Content-Type": "application/json", "X-Tenant-Id": TENANT, "X-Principal-Id": principal,
        "X-Legal-Entity-Id": ENTITY, "X-Correlation-ID": corr, "X-Request-Id": uuid.uuid4().hex,
        "X-Source-Channel": "system", "X-Purpose-Context": "decision_log_writer_probe",
        "Idempotency-Key": "dl1-probe:" + decision_id})
    try:
        with urllib.request.urlopen(req, timeout=15, context=ctx) as resp:
            return resp.status, resp.read()[:300]
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:300]


failures = 0
for service, principal, action, outcome in WRITERS:
    try:
        paths = provision(service)
        status, body = post_decision(paths, principal, action, outcome)
    except Exception as exc:  # noqa: BLE001 - a probe reports every failure
        status, body = 0, str(exc).encode()
    ok = status in (200, 201)
    failures += 0 if ok else 1
    print(f"{'PASS' if ok else 'FAIL'}  {service:28s} as {principal:28s} -> {status} {body.decode(errors='replace') if not ok else ''}")
print(f"{len(WRITERS) - failures} pass / {failures} fail")
sys.exit(failures)
