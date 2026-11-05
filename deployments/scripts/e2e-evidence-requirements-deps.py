#!/usr/bin/env python3
"""End-to-end check of evidence-requirements-svc and every service it depends on.

Each check crosses one real dependency edge of evidence-requirements-svc on the
local compose stack, so a failure names the edge that is broken:

  access-control-svc          HTTP: every action the service checks is registered
  identity-context-svc        HTTP: envelope JWKS + session introspection (revocation)
  gateway-auth-svc + Traefik  HTTP: ForwardAuth on /evidence-requirements, verified
                              identity headers replace the client's
  tenant-entity-registry-svc  HTTP: the legal entity of an evaluation
  document-vault-svc          HTTP: SUPPORTING_DOCUMENT references verified
  evidence-manifest-svc       HTTP: EVIDENCE_MANIFEST references verified
  esignature-integration-svc  HTTP: SIGNATURE references verified (fail-closed when down)
  authorization-svc           HTTP: every gate; its decision log shows the warn-mode DENY
  postgres                    the ledger, the outbox, record_version (API-3)
  kafka                       evidence.requirement.* published exactly once
  otel-collector / jaeger     OTLP: traces reach Jaeger

    python deployments/scripts/e2e-evidence-requirements-deps.py [--with-outage]

Run seed-demo-rbac.ps1 and seed-demo-registry.ps1 first. Writes are undone where
the API allows it: requirements are end-dated, evidence requests closed;
documents are left in place, titled e2e-…. A check that cannot run prints SKIP,
is counted separately and is never a pass; no mandatory edge may be skipped.

Exit status is the number of failed checks.
"""
import argparse, base64, json, os, ssl, subprocess, sys, time, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")

TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
APPROVER = "66666666-6666-6666-6666-666666666666"  # second demo principal (same role)
OTHER_TENANT = "99999999-9999-4999-8999-999999999999"

H = "http://127.0.0.1"
EVIDENCE, AUTHZ, IDENTITY, REGISTRY = f"{H}:8130", f"{H}:8089", f"{H}:8080", f"{H}:8081"
ACCESS, VAULT, MANIFEST, ESIGN = f"{H}:8137", f"{H}:8094", f"{H}:8095", f"{H}:8148"
GATEWAY_AUTH, EDGE, JAEGER = f"{H}:8092", "https://127.0.0.1:8443/evidence-requirements", f"{H}:16686"
TOPIC = "zoiko.evidence-requirements.events"
STAMP = uuid.uuid4().hex[:8].upper()

EVIDENCE_ACTIONS = ["EVIDENCE_EVALUATE", "EVIDENCE_REQUIREMENT_READ", "EVIDENCE_EVALUATION_READ",
                    "EVIDENCE_REQUIREMENT_CREATE", "EVIDENCE_REQUIREMENT_RETIRE", "EVIDENCE_REQUEST_CREATE",
                    "EVIDENCE_REQUEST_MANAGE", "EVIDENCE_REQUEST_SUBMIT_RESPONSE", "EVIDENCE_REQUEST_EVALUATE"]

results, skips = [], []
cleanup = []   # (label, fn) run at the end
published = {}  # evaluation_id -> outcome, for the Kafka check
not_published = []  # evaluation ids that must produce no event

_insecure = ssl.create_default_context()
_insecure.check_hostname = False
_insecure.verify_mode = ssl.CERT_NONE


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


def skip(name, reason):
    skips.append((name, reason))
    print(f"SKIP  {name}  — {reason}", flush=True)


def envelope(principal=ADMIN, write=False, tenant=TENANT, entity=ENTITY, jwt=None, if_match=None):
    """The canonical input contract: identity, correlation, channel, and on a
    write an Idempotency-Key."""
    rid = str(uuid.uuid4())
    h = {"X-Tenant-Id": tenant, "X-Principal-Id": principal, "X-Legal-Entity-Id": entity,
         "X-Correlation-ID": "e2e-evr-" + rid[:8], "X-Request-Id": rid, "X-Source-Channel": "api",
         "X-Source-System": "e2e-evidence-requirements-deps"}
    if jwt:
        h["Authorization"] = "Bearer " + jwt
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
    if if_match:
        h["If-Match"] = if_match
    return h


def call(method, url, headers=None, body=None, timeout=15):
    """(status, headers, parsed body). Never raises for an HTTP status."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        ctx = _insecure if url.startswith("https://") else None
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
            status, hdrs, raw = r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        status, hdrs, raw = e.code, e.headers, e.read()
    except (urllib.error.URLError, OSError) as e:
        return 0, {}, {"error": f"unreachable: {e}"}
    try:
        parsed = json.loads(raw) if raw else {}
    except ValueError:
        parsed = {"raw": raw[:200].decode("utf8", "replace")}
    return status, hdrs, parsed


def until(predicate, seconds=30, step=1.0):
    deadline = time.time() + seconds
    while True:
        v = predicate()
        if v[0] or time.time() >= deadline:
            return v
        time.sleep(step)


def brief(body):
    return json.dumps(body)[:220]


def code(body):
    return body.get("code") or body.get("error") if isinstance(body, dict) else None


def psql(db, sql):
    out = subprocess.run(["docker", "exec", "zoiko-postgres", "psql", "-U", "postgres", "-d", db, "-tAX", "-c", sql],
                         capture_output=True, text=True, timeout=30)
    return out.stdout.strip() if out.returncode == 0 else "ERR " + out.stderr.strip()[:200]


def ledger_count():
    return psql("evidence_requirements", f"SELECT count(*) FROM evidence_evaluations WHERE tenant_id = '{TENANT}'")


# ── API helpers ──────────────────────────────────────────────────────────────

def create_requirement(action, evidence_type, minimum=1, entity=None):
    body = {"tenant_id": TENANT, "domain_code": "E2E", "action_type": action, "evidence_type": evidence_type,
            "correlation_id": "e2e-" + uuid.uuid4().hex[:8], "requirement_payload": {"minimum_count": minimum}}
    if entity:
        body["legal_entity_id"] = entity
    h = envelope(write=True)
    s, hd, b = call("POST", f"{EVIDENCE}/v1/admin/evidence-requirements", h, body)
    if s == 201:
        rid = b["evidence_requirement_id"]
        cleanup.append((f"end-date requirement {action}/{evidence_type}", lambda: retire(rid)))
    return s, hd, b, h, body


def retire(rid):
    s, hd, _ = call("GET", f"{EVIDENCE}/v1/evidence-requirements/{rid}", envelope())
    if s != 200:
        return s
    s, _, _ = call("POST", f"{EVIDENCE}/v1/admin/evidence-requirements/{rid}/end-date",
                   envelope(write=True, if_match=hd.get("ETag")), {"reason": "e2e dependency check finished"})
    return s


def evaluate(action, artifacts=(), principal=ADMIN, entity=ENTITY, key=None, base=EVIDENCE, jwt=None, tenant=TENANT):
    h = envelope(principal, write=True, entity=entity, jwt=jwt, tenant=tenant)
    if key:
        h["Idempotency-Key"] = key
    body = {"legal_entity_id": entity, "domain_code": "E2E", "action_type": action,
            "correlation_id": "e2e-" + uuid.uuid4().hex[:8], "present_artifacts": list(artifacts)}
    s, hd, b = call("POST", f"{base}/v1/evidence/evaluate", h, body)
    return s, b, h["Idempotency-Key"], body


# ── 0. every service is up ───────────────────────────────────────────────────

def health():
    for name, url in [
        ("evidence-requirements-svc", f"{EVIDENCE}/readyz"),
        ("authorization-svc", f"{AUTHZ}/readyz"),
        ("identity-context-svc", f"{IDENTITY}/health"),
        ("tenant-entity-registry-svc", f"{REGISTRY}/healthz"),
        ("access-control-svc", f"{ACCESS}/healthz"),
        ("document-vault-svc", f"{VAULT}/healthz"),
        ("evidence-manifest-svc", f"{MANIFEST}/healthz"),
        ("gateway-auth-svc", f"{GATEWAY_AUTH}/healthz"),
        ("traefik (zoiko-gateway)", "http://127.0.0.1:8091/api/overview"),
        ("jaeger", f"{JAEGER}/api/services"),
    ]:
        s, _, _ = call("GET", url, timeout=5)
        check(f"health {name}", s == 200, f"HTTP {s}" if s != 200 else "")


# ── 1. access-control-svc: every checked action is registered (ER-1) ─────────

def access_control():
    s, _, b = call("GET", f"{ACCESS}/v1/permission-definitions?page_size=500", envelope())
    items = b if isinstance(b, list) else (b.get("items") or b.get("data") or b.get("permission_definitions") or [])
    names = {i.get("action_name") for i in items if isinstance(i, dict)}
    missing = [a for a in EVIDENCE_ACTIONS if a not in names]
    check("access-control: every evidence action is a registered permission", s == 200 and not missing,
          f"HTTP {s} missing={missing}" if missing or s != 200 else f"{len(EVIDENCE_ACTIONS)} actions")


# ── 2. identity-context-svc ───────────────────────────────────────────────────

def login(password):
    s, _, b = call("POST", f"{IDENTITY}/v1/authenticate", {"Content-Type": "application/json"},
                   {"tenant_id": TENANT, "email": "admin@zoikosuite.com", "password": password,
                    "client_id": os.environ.get("IDENTITY_CONSOLE_CLIENT_ID", "zoiko-console"),
                    "client_secret": os.environ.get("IDENTITY_CONSOLE_CLIENT_SECRET", "local-dev-console-client-secret")})
    if s != 200 or not b.get("access_token"):
        return None, None, f"login HTTP {s} {brief(b)}"
    s, _, r = call("POST", f"{IDENTITY}/v1/context/resolve", envelope(write=True),
                   {"bearer_token": b["access_token"], "legal_entity_id": ENTITY})
    if not r.get("envelope_jwt"):
        return None, None, f"resolve HTTP {s} {brief(r)}"
    return r["envelope_jwt"], r.get("session_context_id"), ""


def identity(password):
    jwt, sid, why = login(password)
    if not check("identity: password login resolved to a signed envelope", jwt, why):
        return None
    s, hd, b = call("GET", f"{EVIDENCE}/v1/evidence/summary", envelope(jwt=jwt))
    check("identity: the verified envelope is admitted", s == 200 and (hd.get("X-Identity-Assertion") or "") != "missing",
          f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")
    head, payload, sig = jwt.split(".")
    tampered = ".".join([head, payload, sig[:-4] + ("AAAA" if not sig.endswith("AAAA") else "BBBB")])
    s, _, b = call("GET", f"{EVIDENCE}/v1/evidence/summary", envelope(jwt=tampered))
    check("identity: a tampered envelope is 401 (warn mode too)", s == 401, f"HTTP {s} {code(b)}")

    # A second session, invalidated at identity-context, is refused within the TTL.
    jwt2, sid2, why = login(password)
    if not check("identity: second session for the revocation check", jwt2 and sid2, why or "no session_context_id"):
        return jwt
    s, _, _ = call("GET", f"{EVIDENCE}/v1/evidence/summary", envelope(jwt=jwt2))
    check("identity: the second session is admitted before invalidation", s == 200, f"HTTP {s}")
    s, _, b = call("POST", f"{IDENTITY}/v1/context/session/{sid2}/invalidate", envelope(write=True, jwt=jwt2), {"reason": "ADMIN_REVOKE"})
    if check("identity: session invalidated", s in (200, 204), f"HTTP {s} {brief(b)}" if s not in (200, 204) else ""):
        t0 = time.time()
        while True:
            s, _, b = call("GET", f"{EVIDENCE}/v1/evidence/summary", envelope(jwt=jwt2))
            if s == 401 or time.time() - t0 > 60:
                break
            time.sleep(0.5)
        check("identity: the invalidated session is refused within the revocation TTL", s == 401,
              f"HTTP {s} {code(b)}; lag {time.time() - t0:.1f}s")
    return jwt


# ── 3. Traefik + gateway-auth-svc ─────────────────────────────────────────────

def edge(jwt):
    s, hd, b = call("GET", f"{EDGE}/v1/evidence/summary", {"X-Request-Id": str(uuid.uuid4())})
    check("edge: no bearer is 401 from the gateway", s == 401 and (hd.get("X-Auth-Denial-Reason") or "") == "no_token",
          f"HTTP {s} X-Auth-Denial-Reason={hd.get('X-Auth-Denial-Reason')}")
    if not jwt:
        skip("edge: verified envelope through Traefik", "no envelope (identity login failed)")
        return
    s, _, b = call("GET", f"{EDGE}/v1/evidence/summary", {"Authorization": "Bearer " + jwt, "X-Request-Id": str(uuid.uuid4())})
    check("edge: the verified envelope reaches the service through Traefik", s == 200, f"HTTP {s} {brief(b)}" if s != 200 else "")

    # A client forging another tenant and principal next to its envelope: the
    # gateway's verified values replace them, so the determination is recorded
    # in the verified tenant for the verified principal.
    action = f"E2E_EDGE_{STAMP}"
    s, b, _, _ = evaluate(action, base=EDGE, jwt=jwt, tenant=OTHER_TENANT, principal="forged-principal")
    eid = b.get("evaluation_id")
    if check("edge: an evaluation with forged X-Tenant-Id / X-Principal-Id is served", s == 200 and eid, f"HTTP {s} {brief(b)}"):
        not_published.append(eid)  # NO_REQUIREMENTS_DEFINED
        row = psql("evidence_requirements", f"SELECT tenant_id || '|' || evaluated_for_principal_id FROM evidence_evaluations WHERE evaluation_id = '{eid}'")
        check("edge: the ledger row carries the verified tenant and principal, not the forged ones",
              row.startswith(TENANT + "|") and "forged" not in row, row)


# ── 4. core evaluate and idempotency ──────────────────────────────────────────

def core():
    action = f"E2E_CORE_{STAMP}"
    s, b, _, _ = evaluate(action)
    if check("core: a fresh action_type is NO_REQUIREMENTS_DEFINED", s == 200 and b.get("outcome") == "NO_REQUIREMENTS_DEFINED",
             f"HTTP {s} {b.get('outcome')}"):
        not_published.append(b["evaluation_id"])

    s, hd, r, h, body = create_requirement(action, "APPROVAL_RECORD")
    check("core: requirement created (201, ETag v1)", s == 201 and hd.get("ETag") == '"v1"', f"HTTP {s} ETag={hd.get('ETag')} {brief(r) if s != 201 else ''}")
    s2, _, r2 = call("POST", f"{EVIDENCE}/v1/admin/evidence-requirements", h, body)
    check("core: requirement replay is 200 with the same id", s2 == 200 and r2.get("evidence_requirement_id") == r.get("evidence_requirement_id"), f"HTTP {s2}")
    s3, _, r3 = call("POST", f"{EVIDENCE}/v1/admin/evidence-requirements", h, {**body, "evidence_type": "OTHER_RECORD"})
    check("core: same key, different requirement is 409 idempotency_mismatch", s3 == 409 and code(r3) == "idempotency_mismatch", f"HTTP {s3} {code(r3)}")

    s, b, key, ebody = evaluate(action, [{"evidence_type": "APPROVAL_RECORD", "reference_id": "approval-" + STAMP}])
    if check("core: SATISFIED with the required artifact", s == 200 and b.get("outcome") == "SATISFIED", f"HTTP {s} {brief(b)}"):
        published[b["evaluation_id"]] = "SATISFIED"
    first = b.get("evaluation_id")
    h = envelope(write=True)
    h["Idempotency-Key"] = key
    s, _, b = call("POST", f"{EVIDENCE}/v1/evidence/evaluate", h, ebody)
    check("core: evaluate replay is 200 with the same evaluation_id", s == 200 and b.get("evaluation_id") == first, f"HTTP {s} {b.get('evaluation_id')}")
    s, _, b = call("POST", f"{EVIDENCE}/v1/evidence/evaluate", h, {**ebody, "action_type": action + "_OTHER"})
    check("core: same key, different action is 409 idempotency_mismatch", s == 409 and code(b) == "idempotency_mismatch", f"HTTP {s} {code(b)}")

    s, b, _, _ = evaluate(action, [{"evidence_type": "APPROVAL_RECORD", "reference_id": "  "}])
    if check("core: an empty reference does not count (MISSING)", s == 200 and b.get("outcome") == "MISSING", f"HTTP {s} {b.get('outcome')}"):
        published[b["evaluation_id"]] = "MISSING"


# ── 5. tenant-entity-registry-svc ─────────────────────────────────────────────

def registry():
    before = ledger_count()
    other = psql("tenant_entity_registry", f"SELECT legal_entity_id FROM legal_entities WHERE tenant_id <> '{TENANT}' LIMIT 1")
    if other and not other.startswith("ERR"):
        s, b, _, _ = evaluate(f"E2E_REG_{STAMP}", entity=other)
        check("registry: a legal entity of another tenant is refused (422 legal_entity_not_found)",
              s == 422 and code(b) == "legal_entity_not_found", f"HTTP {s} {code(b)}")
    else:
        skip("registry: a legal entity of another tenant", "the registry seed holds no second tenant's entity")
    s, b, _, _ = evaluate(f"E2E_REG_{STAMP}", entity=str(uuid.uuid4()))
    check("registry: a random legal entity is refused (422 legal_entity_not_found)", s == 422 and code(b) == "legal_entity_not_found", f"HTTP {s} {code(b)}")
    check("registry: neither refusal wrote a ledger row", ledger_count() == before, f"before={before} after={ledger_count()}")


# ── 6. document-vault-svc ─────────────────────────────────────────────────────

PLATFORM_ENTITY = "00000000-0000-0000-0000-00000000f001"  # the demo admin's platform-scope assignment


def create_document(entity=ENTITY):
    """A document through document-vault-svc's API; (status, document_id, body)."""
    h = envelope(write=True, entity=entity)
    # document-vault-svc requires a purpose on every write (canonical input contract).
    h["X-Purpose-Context"] = "evidence-requirements e2e: a supporting document to verify"
    s, _, b = call("POST", f"{VAULT}/v1/documents", h, {
        "legal_entity_id": entity, "title": f"e2e-evidence-{STAMP}", "classification": "INTERNAL",
        "content_type": "text/plain", "content_base64": base64.b64encode(f"e2e evidence {STAMP}".encode()).decode()})
    doc = b.get("document", b) if isinstance(b, dict) else {}
    return s, doc.get("document_id"), b


def fixture_document(entity):
    """A document row written straight into document_vault, for a stack whose
    vault refuses every upload (no clamd). The verification edge -- the
    envelope forwarded to GET /v1/documents/{id} and the vault's own
    DOCUMENT_READ check -- runs exactly as for an uploaded one."""
    out = psql("document_vault", "INSERT INTO documents (tenant_id, legal_entity_id, title, classification, created_by_principal_id) "
               f"VALUES ('{TENANT}', '{entity}', 'e2e-evidence-fixture-{STAMP}', 'INTERNAL', '{ADMIN}') RETURNING document_id")
    return out.splitlines()[0] if out and not out.startswith("ERR") else None


def document_vault(with_outage):
    s, doc, b = create_document()
    if s == 503 and code(b) == "scan_unavailable":
        skip("document-vault: a document uploaded through its API",
             "the vault refuses every upload without clamd, and compose runs none (BLOCKED: owner environment); a fixture row is used instead")
        doc = fixture_document(ENTITY)
        if not check("document-vault: fixture document in the demo entity", doc, "insert failed"):
            return
    elif not check("document-vault: a real document created", s == 201 and doc, f"HTTP {s} {brief(b)}"):
        return
    one, two = f"E2E_DOC1_{STAMP}", f"E2E_DOC2_{STAMP}"
    create_requirement(one, "SUPPORTING_DOCUMENT", 1)
    create_requirement(two, "SUPPORTING_DOCUMENT", 2)
    s, b, _, _ = evaluate(one, [{"evidence_type": "SUPPORTING_DOCUMENT", "reference_id": doc}])
    if check("document-vault: the document is verified with the vault — SATISFIED", s == 200 and b.get("outcome") == "SATISFIED", f"HTTP {s} {brief(b)}"):
        published[b["evaluation_id"]] = "SATISFIED"
    s, b, _, _ = evaluate(one, [{"evidence_type": "SUPPORTING_DOCUMENT", "reference_id": str(uuid.uuid4())}])
    reason = " ".join(u.get("reason", "") for u in b.get("unmet") or [])
    if check("document-vault: a random id is MISSING with the reason", s == 200 and b.get("outcome") == "MISSING" and "does not exist" in reason,
             f"HTTP {s} {b.get('outcome')} {reason[:120]}"):
        published[b["evaluation_id"]] = "MISSING"
    s, b, _, _ = evaluate(two, [{"evidence_type": "SUPPORTING_DOCUMENT", "reference_id": doc},
                                {"evidence_type": "SUPPORTING_DOCUMENT", "reference_id": doc.upper()}])
    if check("document-vault: one document twice against minimum_count 2 is MISSING", s == 200 and b.get("outcome") == "MISSING",
             f"HTTP {s} {b.get('outcome')}"):
        published[b["evaluation_id"]] = "MISSING"
    # A document the caller may read, but of another legal entity: the vault
    # answers it, and it must not count for this entity's action.
    other = fixture_document(PLATFORM_ENTITY)
    if other:
        s, b, _, _ = evaluate(one, [{"evidence_type": "SUPPORTING_DOCUMENT", "reference_id": other}])
        reason = " ".join(u.get("reason", "") for u in b.get("unmet") or [])
        if check("document-vault: a document of another legal entity is MISSING", s == 200 and b.get("outcome") == "MISSING"
                 and "different tenant or legal entity" in reason, f"HTTP {s} {b.get('outcome')} {reason[:120]}"):
            published[b["evaluation_id"]] = "MISSING"
    else:
        skip("document-vault: a document of another legal entity", "could not write the fixture row")

    if with_outage:
        before = ledger_count()
        subprocess.run(["docker", "stop", "document-vault-svc"], capture_output=True, timeout=60)
        try:
            s, b, _, _ = evaluate(one, [{"evidence_type": "SUPPORTING_DOCUMENT", "reference_id": doc}])
            check("document-vault outage: 503 document_service_unavailable", s == 503 and code(b) == "document_service_unavailable", f"HTTP {s} {code(b)}")
            check("document-vault outage: no ledger row written", ledger_count() == before, f"before={before} after={ledger_count()}")
        finally:
            subprocess.run(["docker", "start", "document-vault-svc"], capture_output=True, timeout=60)
            until(lambda: (call("GET", f"{VAULT}/healthz", timeout=3)[0] == 200, None), seconds=60, step=2)


# ── 7. evidence-manifest-svc ──────────────────────────────────────────────────

def evidence_manifest():
    action = f"E2E_MAN_{STAMP}"
    create_requirement(action, "EVIDENCE_MANIFEST", 1)
    s, b, _, _ = evaluate(action, [{"evidence_type": "EVIDENCE_MANIFEST", "reference_id": str(uuid.uuid4())}])
    reason = " ".join(u.get("reason", "") for u in b.get("unmet") or [])
    if check("evidence-manifest: an unknown manifest is MISSING with the reason",
             s == 200 and b.get("outcome") == "MISSING" and "evidence-manifest-svc" in reason, f"HTTP {s} {brief(b)}"):
        published[b["evaluation_id"]] = "MISSING"
    real = psql("evidence_manifest", f"SELECT manifest_id FROM evidence_manifests WHERE tenant_id = '{TENANT}' AND legal_entity_id = '{ENTITY}' LIMIT 1")
    if not real or real.startswith("ERR"):
        skip("evidence-manifest: a manifest generated through its API",
             "generating one needs governance-svc, workflow-svc and workflow-history-svc (BLOCKED: owner seed); a fixture row is used instead")
        real = psql("evidence_manifest", "INSERT INTO evidence_manifests (tenant_id, legal_entity_id, scenario_type, requested_by, status, generated_at) "
                    f"VALUES ('{TENANT}', '{ENTITY}', 'AUDIT', '{ADMIN}', 'GENERATED', now()) RETURNING manifest_id").splitlines()[0]
    if not check("evidence-manifest: a manifest exists for the demo entity", real and not real.startswith("ERR"), real):
        return
    registered = psql("access_control", "SELECT count(*) FROM permission_definitions WHERE action_name = 'EVIDENCE_MANIFEST_READ'")
    before = ledger_count()
    s, b, _, _ = evaluate(action, [{"evidence_type": "EVIDENCE_MANIFEST", "reference_id": real}])
    if registered == "0":
        # evidence-manifest-svc authorizes every read on EVIDENCE_MANIFEST_READ,
        # which access-control does not register, so the owner refuses (403)
        # for every caller. That refusal must stop the check, not count.
        check("evidence-manifest: an owner that refuses the read is 503, never counted",
              s == 503 and code(b) == "evidence_manifest_unavailable", f"HTTP {s} {code(b)}")
        check("evidence-manifest: the refusal wrote no ledger row", ledger_count() == before, f"before={before} after={ledger_count()}")
        skip("evidence-manifest: a real manifest counts (SATISFIED)",
             "EVIDENCE_MANIFEST_READ is not registered in access-control, so evidence-manifest-svc refuses every read (BLOCKED: owner R5)")
    elif check("evidence-manifest: the manifest is verified with its owner — SATISFIED", s == 200 and b.get("outcome") == "SATISFIED", f"HTTP {s} {brief(b)}"):
        published[b["evaluation_id"]] = "SATISFIED"


# ── 8. esignature-integration-svc ─────────────────────────────────────────────

def esignature():
    action = f"E2E_SIG_{STAMP}"
    create_requirement(action, "SIGNATURE", 1)
    up = call("GET", f"{ESIGN}/healthz", timeout=3)[0] == 200
    before = ledger_count()
    s, b, _, _ = evaluate(action, [{"evidence_type": "SIGNATURE", "reference_id": str(uuid.uuid4())}])
    if up:
        if check("esignature: an unknown envelope is MISSING", s == 200 and b.get("outcome") == "MISSING", f"HTTP {s} {brief(b)}"):
            published[b["evaluation_id"]] = "MISSING"
    else:
        check("esignature unreachable: a SIGNATURE reference is 503 esignature_unavailable, never counted",
              s == 503 and code(b) == "esignature_unavailable", f"HTTP {s} {code(b)}")
        check("esignature unreachable: no ledger row written", ledger_count() == before, f"before={before} after={ledger_count()}")
        skip("esignature: a real envelope counts / an unknown one is MISSING",
             "esignature-integration-svc runs in the phase7 stack (its own postgres-p7/kafka-p7 network), not up beside this stack (BLOCKED)")


# ── 9. Kafka ──────────────────────────────────────────────────────────────────

def kafka():
    time.sleep(7)  # the outbox worker polls every 5 s
    out = subprocess.run(["docker", "exec", "zoiko-kafka", "/opt/kafka/bin/kafka-console-consumer.sh",
                          "--bootstrap-server", "localhost:9094", "--topic", TOPIC, "--from-beginning",
                          "--timeout-ms", "15000", "--property", "print.key=true", "--property", "key.separator=\t"],
                         capture_output=True, text=True, timeout=90)
    keys = {}
    for line in out.stdout.splitlines():
        k, _, v = line.partition("\t")
        keys.setdefault(k, []).append(v)
    if not check("kafka: topic readable", out.returncode == 0 or keys, (out.stderr or "")[-200:] if not keys else f"{len(keys)} keys"):
        return
    for eid, outcome in published.items():
        msgs = keys.get(f"{TENANT}/{eid}", [])
        ok = len(msgs) == 1 and f"evidence.requirement.{outcome.lower()}" in msgs[0]
        check(f"kafka: {outcome} {eid[:8]} published exactly once, key <tenant>/<evaluation_id>", ok, f"{len(msgs)} message(s)")
    stray = [e for e in not_published if keys.get(f"{TENANT}/{e}")]
    check("kafka: NO_REQUIREMENTS_DEFINED and replays published nothing", not stray, f"stray={stray}")
    ids = ",".join(f"'{e}'" for e in published)
    if ids:
        n = psql("evidence_requirements", f"SELECT count(*) FILTER (WHERE published_at IS NULL) || '/' || count(*) FROM outbox WHERE aggregate_id IN ({ids})")
        check("outbox: every row of this run is published", n.startswith("0/"), n)


# ── 10. authorization-svc ─────────────────────────────────────────────────────

def authorization(subject):
    h = envelope(subject, write=True)
    s, _, b = call("POST", f"{EVIDENCE}/v1/admin/evidence-requirements", h, {
        "tenant_id": TENANT, "domain_code": "E2E", "action_type": f"E2E_DENY_{STAMP}", "evidence_type": "APPROVAL_RECORD",
        "correlation_id": "e2e-deny"})
    check("authz: the unprivileged subject is refused catalogue create (403)", s == 403, f"HTTP {s} {code(b)}")
    s, _, b = call("GET", f"{EVIDENCE}/v1/evidence-requirements", envelope(subject))
    check("authz: the unprivileged subject is refused the catalogue read (403)", s == 403, f"HTTP {s} {code(b)}")
    s, _, b = call("GET", f"{EVIDENCE}/v1/evidence/evaluations", envelope(subject))
    check("authz: the unprivileged subject is refused the evaluation read (403)", s == 403, f"HTTP {s} {code(b)}")
    s, _, b = call("GET", f"{EVIDENCE}/v1/pbc/requests", envelope(subject))
    check("authz: the unprivileged subject is refused the PBC register (403)", s == 403, f"HTTP {s} {code(b)}")

    since = datetime.now(timezone.utc) - timedelta(seconds=2)
    s, b, _, _ = evaluate(f"E2E_WARN_{STAMP}", principal=subject)
    if check("authz: evaluate is admitted for the subject in warn mode", s == 200, f"HTTP {s} {code(b)}"):
        not_published.append(b["evaluation_id"])
    row = psql("authorization_svc", "SELECT decision_outcome FROM access_decision_log WHERE principal_id = '%s' AND action_type = 'EVIDENCE_EVALUATE' "
               "AND decided_at >= '%s' ORDER BY decided_at DESC LIMIT 1" % (subject, since.isoformat()))
    check("authz: the decision log records the warn-mode DENY for EVIDENCE_EVALUATE", row in ("DENY", "DENIED"), row or "no row")


# ── 11. PBC end to end, API-3 on every command ────────────────────────────────

def pbc(subject):
    due = (datetime.now(timezone.utc) + timedelta(days=7)).isoformat()
    s, hd, req = call("POST", f"{EVIDENCE}/v1/pbc/requests", envelope(write=True), {
        "legal_entity_id": ENTITY, "title": f"e2e-pbc-{STAMP}", "description": "dependency check",
        "assigned_to_principal_id": APPROVER, "due_at": due, "correlation_id": "e2e-pbc-" + STAMP})
    rid = req.get("request_id")
    if not check("pbc: request created (201, ETag v1)", s == 201 and rid and hd.get("ETag") == '"v1"', f"HTTP {s} {hd.get('ETag')} {brief(req) if s != 201 else ''}"):
        return
    etag = hd.get("ETag")

    def cmd(path, body=None, principal=ADMIN, if_match=None, extra=None):
        h = envelope(principal, write=True, if_match=if_match)
        h.update(extra or {})
        return call("POST", f"{EVIDENCE}/v1/pbc/requests/{rid}{path}", h, body)

    try:
        s, _, b = cmd("/send")
        check("pbc: send without If-Match is 428", s == 428 and code(b) == "precondition_required", f"HTTP {s} {code(b)}")
        s, hd, b = cmd("/send", if_match=etag)
        check("pbc: send with the current If-Match (v2)", s == 200 and hd.get("ETag") == '"v2"', f"HTTP {s} {hd.get('ETag')} {code(b)}")
        stale, etag = etag, hd.get("ETag")
        s, hd, b = cmd("/reassign", {"new_assignee_principal_id": APPROVER}, if_match=stale)
        check("pbc: a stale If-Match is 412 with the current ETag", s == 412 and code(b) == "precondition_failed" and hd.get("ETag") == etag,
              f"HTTP {s} {code(b)} ETag={hd.get('ETag')}")
        s, hd, b = cmd("/extend-due-date", {"new_due_at": (datetime.now(timezone.utc) + timedelta(days=10)).isoformat()}, if_match=etag)
        check("pbc: extend due date (v3)", s == 200 and hd.get("ETag") == '"v3"', f"HTTP {s} {hd.get('ETag')} {code(b)}")
        etag = hd.get("ETag")

        s, _, resp = cmd("/responses", {"correlation_id": "e2e-resp-" + STAMP}, principal=APPROVER)
        rsp = resp.get("response_id")
        check("pbc: the assignee submits a response (no If-Match)", s == 201 and rsp, f"HTTP {s} {brief(resp)}")
        s, hd, _ = call("GET", f"{EVIDENCE}/v1/pbc/requests/{rid}", envelope())
        check("pbc: the response moved the request and bumped its version", hd.get("ETag") == '"v4"', f"ETag={hd.get('ETag')}")
        s, _, b = cmd("/mark-satisfied", if_match=hd.get("ETag"), principal=APPROVER)
        check("pbc: the submitter cannot mark it satisfied (403 self_evaluation_not_allowed)", s == 403 and code(b) == "self_evaluation_not_allowed",
              f"HTTP {s} {code(b)}")
        s, _, b = cmd("/mark-satisfied", if_match=hd.get("ETag"))
        check("pbc: mark satisfied before a clean scan is 422 artifact_not_scanned", s == 422 and code(b) == "artifact_not_scanned", f"HTTP {s} {code(b)}")
        s, _, b = call("POST", f"{EVIDENCE}/v1/pbc/responses/{rsp}/scan-result", envelope(subject, write=True), {"result": "CLEAN"})
        check("pbc: a scan result from a principal without EVIDENCE_REQUEST_MANAGE is 403", s == 403, f"HTTP {s} {code(b)}")
        s, _, b = call("POST", f"{EVIDENCE}/v1/pbc/responses/{rsp}/scan-result", envelope(write=True), {"result": "CLEAN"})
        check("pbc: scan result CLEAN recorded by a manager", s == 200, f"HTTP {s} {code(b)}")
        s, hd, b = cmd("/mark-satisfied", if_match='"v4"')
        check("pbc: marked satisfied by another principal (v5)", s == 200 and b.get("status") == "SATISFIED" and hd.get("ETag") == '"v5"',
              f"HTTP {s} {b.get('status')} {hd.get('ETag')} {code(b)}")
        etag = hd.get("ETag")
    finally:
        s, hd, _ = call("GET", f"{EVIDENCE}/v1/pbc/requests/{rid}", envelope())
        s, _, b = cmd("/close", if_match=hd.get("ETag"))
        check("pbc: request closed (undo)", s == 200 and b.get("status") == "CLOSED", f"HTTP {s} {code(b)}")


# ── 12. console read models (API-4) ───────────────────────────────────────────

def lists():
    s, _, b = call("GET", f"{EVIDENCE}/v1/evidence/evaluations?page_size=2&filter[domain_code]=E2E", envelope())
    nxt = b.get("next_cursor")
    check("lists: first page of evaluations with a next cursor", s == 200 and len(b.get("items", [])) == 2 and nxt, f"HTTP {s} {brief(b)[:120]}")
    if nxt:
        s, _, b2 = call("GET", f"{EVIDENCE}/v1/evidence/evaluations?page_size=2&filter[domain_code]=E2E&cursor={urllib.request.quote(nxt)}", envelope())
        ids1 = {i["evaluation_id"] for i in b.get("items", [])}
        check("lists: the next page continues without repeating", s == 200 and not ids1 & {i["evaluation_id"] for i in b2.get("items", [])}, f"HTTP {s}")
        s, _, b3 = call("GET", f"{EVIDENCE}/v1/evidence/evaluations?page_size=2&filter[domain_code]=E2E&cursor={urllib.request.quote(nxt[:-2] + 'xx')}", envelope())
        check("lists: a tampered cursor is 400 invalid_cursor", s == 400 and code(b3) == "invalid_cursor", f"HTTP {s} {code(b3)}")
    s, _, b = call("GET", f"{EVIDENCE}/v1/evidence-requirements?filter[colour]=red", envelope())
    check("lists: an unknown filter is 400 unknown_filter", s == 400 and code(b) == "unknown_filter", f"HTTP {s} {code(b)}")
    s, _, b = call("GET", f"{EVIDENCE}/v1/evidence/summary", envelope())
    check("lists: the summary reads", s == 200 and isinstance(b.get("active_requirements"), int), f"HTTP {s} {brief(b)[:160]}")


# ── 13. metrics and tracing ───────────────────────────────────────────────────

def telemetry():
    req = urllib.request.Request(f"{EVIDENCE}/metrics")
    try:
        raw = urllib.request.urlopen(req, timeout=10).read().decode()
    except OSError as e:
        raw = str(e)
    for series in ("evidence_evaluations_total", "evidence_owner_unavailable_total", "evidence_outbox_unpublished"):
        check(f"metrics: {series} is served", series in raw)

    def listed():
        s, _, b = call("GET", f"{JAEGER}/api/services")
        return ("evidence-requirements-svc" in (b.get("data") or []), b.get("data"))
    found, services = until(listed, seconds=30, step=3)
    check("tracing: evidence-requirements-svc traces in Jaeger", found, "" if found else f"services={services}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--subject", default=os.environ.get("E2E_SUBJECT", "798980b8-d26e-4439-a065-a7f80120c36e"),
                    help="a principal in the demo tenant that holds no evidence grant")
    ap.add_argument("--password", default=os.environ.get("E2E_ADMIN_PASSWORD", "Zoiko@Governance1"))
    ap.add_argument("--with-outage", action="store_true", help="stop document-vault-svc for one evaluation, then start it")
    a = ap.parse_args()

    health()
    access_control()
    jwt = identity(a.password)
    edge(jwt)
    core()
    registry()
    document_vault(a.with_outage)
    evidence_manifest()
    esignature()
    authorization(a.subject)
    pbc(a.subject)
    lists()
    kafka()
    telemetry()

    for label, fn in reversed(cleanup):
        s = fn()
        if s != 200:
            print(f"note  undo failed: {label} (HTTP {s})")

    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} passed, {len(skips)} skipped")
    for name, reason in skips:
        print(f"  skipped: {name} — {reason}")
    sys.exit(len(failed))


if __name__ == "__main__":
    main()
