#!/usr/bin/env python3
"""End-to-end check of policy-svc, the ZS-GOV-POL-001 / ZS-GOV-INT-002 policy
contract, and every service policy-svc depends on.

Each check crosses one real dependency edge on the local compose stack, so a
failure names the edge that is broken:

  postgres                    migrations 000001-000013 tracked; canonical decision
                              columns; RLS forced; runtime role not a superuser
  access-control-svc          every action policy-svc checks is registered
  authorization-svc           POLICY_* decisions for the demo principals; a
                              stranger is refused (403); maker-checker SoD
  identity-context-svc        a console login's signed envelope is accepted;
                              a tampered one is refused (401)
  mtls-management-svc         governance-decision-log-svc's certificate reaches the
                              :8449 peer listener (replay reads); a workload not
                              in MTLS_ALLOWED_CLIENTS is refused
  governance-decision-log-svc signatory decisions relayed as svc-policy under
                              their canonical outcome and status category
  kafka                       policy.signatory_matrix.published and
                              policy.version.activated on zoiko.policy.events
  workflow-svc                a signature workflow created only from a verified
                              SIGNATURE_REQUIRED decision (over policy-svc :8449);
                              signatures bound to the document hash, signer
                              offices confirmed by authorization-svc at signing,
                              distinct-human office seats, and release re-checked
                              against a fresh decision

and the contract itself (Document 01 §10 P0 cases, Document 02 §13 C02 cases):

  P0-01  unsupported / unconfigured families fail closed (NOT_IMPLEMENTED,
         NOT_CONFIGURED); never PERMIT, never a threshold fallback
  P0-05  signatory quorum bands: 11,000 / 100,000 / 100,000.01 exact
  C02-P0-01/-13  a decision whose signature, outcome or scope cannot be trusted
         is a local ERROR (the checker here re-implements Doc 02 §2.3)
  C02-P0-03  spoofed tenant / entity / actor refused
  C02-P0-04/-09  same request_id replays the same signed bytes; changed content
         is 409, never a merge
  C02-P0-14  a suspended matrix stops governing (NOT_CONFIGURED)
  Doc 01 §5.4  a high-risk instrument needs its own matrix; it coexists with the
         ordinary matrix; a duplicate matrix for one instrument is refused

    python deployments/scripts/e2e-policy-deps.py [--no-kafka] [--no-ledger] [--no-mtls] [--no-workflow]

Run seed-demo-rbac.ps1 first. The script adds the PDC-05 policy actions to the
demo POLICY_FULL bundle (development only) and writes policy families, versions
and decisions that stay (the ledger and evaluations are append-only by design).

Exit status is the number of failed checks.
"""
import argparse, base64, hashlib, http.client, json, os, ssl, subprocess, sys, tempfile, time, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")

TENANT = "11111111-1111-1111-1111-111111111111"
OTHER_TENANT = "99999999-9999-4999-8999-999999999999"
ENTITY = "22222222-2222-2222-2222-222222222222"
OTHER_ENTITY = "44444444-4444-4444-4444-444444444444"
ADMIN = "33333333-3333-3333-3333-333333333333"      # author
APPROVER = "66666666-6666-6666-6666-666666666666"   # checker, activator
PLATFORM = "00000000-0000-0000-0000-00000000f001"
STRANGER = "e2e00000-0000-4000-8000-0000000000bb"

H = "http://127.0.0.1"
POLICY, AUTHZ, ACCESS, IDENTITY = f"{H}:8085", f"{H}:8089", f"{H}:8137", f"{H}:8080"
WORKFLOW = f"{H}:8090"
SIGNER_DIRECTOR = "e2e00000-0000-4000-8000-0000000000c2"   # holds DIRECTOR (dev grant)
SIGNER_NO_OFFICE = "e2e00000-0000-4000-8000-0000000000c3"  # may approve, holds no office
REGISTRY, GDL, MTLS = f"{H}:8081", f"{H}:8083", f"{H}:8140"
POLICY_MTLS = ("127.0.0.1", 8449)
PG, KAFKA, TOPIC = "zoiko-postgres", "zoiko-kafka", "zoiko.policy.events"
NETWORK = "deployments_default"
RUN = uuid.uuid4().hex[:8]
RESOLVE = "/internal/v1/policy/signatory-matrix/resolve"

results = []


def check(name, ok, detail=""):
    results.append((name, "PASS" if ok else "FAIL", detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


def skip(name, why):
    results.append((name, "SKIP", why))
    print(f"SKIP  {name}  — {why}", flush=True)


def envelope(principal=ADMIN, write=False, tenant=TENANT, entity=ENTITY):
    rid = str(uuid.uuid4())
    h = {
        "X-Tenant-Id": tenant,
        "X-Principal-Id": principal,
        "X-Correlation-ID": f"e2e-policy-{RUN}-{rid[:8]}",
        "X-Request-Id": rid,
        "X-Source-Channel": "api",
        "X-Source-System": "e2e-policy-deps",
    }
    if entity:
        h["X-Legal-Entity-Id"] = entity
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
        h["X-Occurred-At"] = datetime.now(timezone.utc).isoformat()
    return h


def call(method, url, headers=None, body=None, timeout=20, raw_body=None):
    data = raw_body if raw_body is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status, hdrs, content = r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        status, hdrs, content = e.code, e.headers, e.read()
    except (urllib.error.URLError, OSError) as e:
        return 0, {}, {"error": f"unreachable: {e}"}, b""
    try:
        parsed = json.loads(content) if content else {}
    except ValueError:
        parsed = {"raw": content[:200].decode("utf8", "replace")}
    return status, hdrs, parsed, content


def pol(method, path, principal=ADMIN, body=None, write=None, entity=ENTITY, tenant=TENANT, extra=None, raw_body=None):
    h = envelope(principal, write=(method != "GET") if write is None else write, tenant=tenant, entity=entity)
    h.update(extra or {})
    return call(method, POLICY + path, h, body, raw_body=raw_body)


def until(predicate, seconds=30, step=1.0):
    deadline = time.time() + seconds
    while True:
        v = predicate()
        if v[0] or time.time() >= deadline:
            return v
        time.sleep(step)


def psql(sql, db="policy"):
    r = subprocess.run(["docker", "exec", "-i", PG, "psql", "-X", "-q", "-U", "postgres", "-d", db, "-tA", "-c", sql],
                       capture_output=True)
    return r.returncode, r.stdout.decode("utf8", "replace").strip(), r.stderr.decode("utf8", "replace").strip()


def brief(body):
    return json.dumps(body)[:260]


# ── Doc 02 §2.3: the caller-side contract check, re-implemented independently ──

OUTCOMES = {"PERMIT", "APPROVAL_REQUIRED", "SIGNATURE_REQUIRED", "REVIEW_REQUIRED", "DENY",
            "NOT_CONFIGURED", "NOT_IMPLEMENTED", "ERROR"}
_KEYS = {}


def canonical(obj):
    """Go's canonical form: sorted keys, compact, no HTML or ASCII escaping."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf8")


def decision_keys():
    if not _KEYS:
        s, _, b, _ = pol("GET", "/v1/policy-decision-keys")
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
        for k in b.get("keys", []):
            x = base64.urlsafe_b64decode(k["x"] + "=" * (-len(k["x"]) % 4))
            _KEYS[k["kid"]] = Ed25519PublicKey.from_public_bytes(x)
    return _KEYS


def verify_signature(raw):
    d = json.loads(raw)
    integ = d.pop("integrity", None) or {}
    key = decision_keys().get(integ.get("kid"))
    if integ.get("alg") != "EdDSA" or key is None:
        return False
    sig = base64.urlsafe_b64decode(integ.get("value", "") + "=" * (-len(integ.get("value", "")) % 4))
    try:
        key.verify(sig, canonical(d))
        return True
    except Exception:
        return False


def contract_check(raw, tenant=TENANT, entity=ENTITY, actor=ADMIN, action="SIGN_CONTRACT", resource=None):
    """(outcome to act on, problems): ERROR unless every Doc 02 §2.3 check passes."""
    try:
        d = json.loads(raw)
    except ValueError:
        return "ERROR", ["not JSON"]
    p = []
    if d.get("outcome") not in OUTCOMES:
        p.append("unknown outcome")
    if not str(d.get("contract_version", "")).startswith("1."):
        p.append("contract version")
    for f in ("decision_id", "owning_service", "rule_id", "input_fingerprint", "evidence_ref", "evaluated_at", "valid_until"):
        if not d.get(f):
            p.append(f + " missing")
    if not d.get("reason_codes"):
        p.append("no reason codes")
    for f, want in (("tenant_id", tenant), ("legal_entity_id", entity), ("actor_principal_id", actor), ("action", action),
                    ("owning_service", "policy-svc")):
        if d.get(f) != want:
            p.append(f"{f} {d.get(f)} != {want}")
    if resource and d.get("resource") != resource:
        p.append("resource mismatch")
    now = datetime.now(timezone.utc)
    try:
        if datetime.fromisoformat(d["valid_until"].replace("Z", "+00:00")) <= now:
            p.append("expired")
        if datetime.fromisoformat(d["evaluated_at"].replace("Z", "+00:00")) > now + timedelta(seconds=30):
            p.append("future-dated")
    except (KeyError, ValueError):
        p.append("bad timestamps")
    if not verify_signature(raw):
        p.append("signature")
    return ("ERROR" if p else d["outcome"]), p


# ── fixtures (Doc 01 §5.2 example bands: test fixtures, not a live mandate) ──

MATRIX = {
    "currency": "GBP", "valuation_basis": "GROSS",
    "instrument_types": ["SUPPLIER_CONTRACT", "PAYMENT_INSTRUCTION", "NDA"],
    "legal_review_risk_flags": ["NONSTANDARD_DATA_PROCESSING"],
    "bands": [
        {"band_id": "B1", "above": None, "up_to": "10000", "min_distinct_signers": 1,
         "required_offices": [{"office": "FINANCE_SIGNER", "min_count": 1}]},
        {"band_id": "B2", "above": "10000", "up_to": "100000", "min_distinct_signers": 2,
         "required_offices": [{"office": "DIRECTOR", "min_count": 1}],
         "eligible_offices": ["FINANCE_SIGNER", "DIRECTOR", "CFO", "CEO"], "distinct_departments": True},
        {"band_id": "B3", "above": "100000", "up_to": None, "min_distinct_signers": 2,
         "required_offices": [{"office": "CFO", "min_count": 1}, {"office": "CEO", "min_count": 1}],
         "sequencing": "SEQUENTIAL", "order": ["CFO", "CEO"], "separate_release_authorization": True},
    ],
    "exemptions": [{"exemption_id": "E-NDA", "instrument_type": "NDA", "reason": "mutual NDA on the published template"}],
}
HIGH_RISK_MATRIX = {
    "currency": "GBP", "valuation_basis": "GROSS", "high_risk": True,
    "instrument_types": ["GUARANTEE", "SUPPLIER_CONTRACT"],
    "bands": [{"band_id": "H1", "above": None, "up_to": None, "min_distinct_signers": 2,
               "required_offices": [{"office": "DIRECTOR", "min_count": 2}], "legal_review": True}],
}
DOC_HASH = "sha256:" + hashlib.sha256(f"e2e contract {RUN}".encode()).hexdigest()


def resolve_body(amount, request_id=None, **over):
    b = {
        "contract_version": "1.0", "request_id": request_id or f"e2e-sig-{RUN}-{uuid.uuid4().hex[:8]}",
        "correlation_id": f"e2e-flow-{RUN}", "tenant_id": TENANT, "legal_entity_id": ENTITY,
        "actor_principal_id": ADMIN, "action": "SIGN_CONTRACT",
        "resource": {"type": "contract", "id": f"C-{RUN}", "version": "3"},
        "money": {"amount": amount, "currency": "GBP", "basis": "GROSS"},
        "instrument_type": "SUPPLIER_CONTRACT", "jurisdiction": "GB", "document_hash": DOC_HASH,
        "evaluated_for_at": datetime.now(timezone.utc).isoformat(),
    }
    b.update(over)
    return b


def resolve(body, **kw):
    return pol("POST", RESOLVE, body=body, write=False, **kw)


# ── 0. every service is up ───────────────────────────────────────────────────

def health():
    for name, url in [("policy-svc", f"{POLICY}/readyz"), ("authorization-svc", f"{AUTHZ}/healthz"),
                      ("access-control-svc", f"{ACCESS}/healthz"), ("identity-context-svc", f"{IDENTITY}/health"),
                      ("tenant-entity-registry-svc", f"{REGISTRY}/healthz"), ("governance-decision-log-svc", f"{GDL}/healthz"),
                      ("mtls-management-svc", f"{MTLS}/healthz")]:
        s, _, b, _ = call("GET", url, timeout=5)
        check(f"health {name}", s == 200, f"HTTP {s}" if s != 200 else "")


# ── 1. postgres: schema and migrations ────────────────────────────────────────

def schema():
    rc, out, err = psql("SELECT count(*) FROM schema_migrations;")
    check("postgres: policy migrations 000001-000013 all tracked", rc == 0 and out == "15", out or err)
    rc, out, _ = psql("SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns "
                      "WHERE table_name='policy_evaluations' AND column_name IN ('decision_envelope','decision_outcome','reason_codes','valid_until','contract_version');")
    check("postgres: 000013 canonical decision columns present", out == "contract_version,decision_envelope,decision_outcome,reason_codes,valid_until", out)
    rc, out, _ = psql("SELECT is_nullable FROM information_schema.columns WHERE table_name='policy_evaluations' AND column_name='policy_version_id';")
    check("postgres: evidence of a NOT_CONFIGURED decision may lack a version (000013)", out == "YES", out)
    rc, out, _ = psql("SELECT bool_and(relforcerowsecurity) FROM pg_class WHERE relname IN ('policy_evaluations','policy_versions','policies','outbox','policy_evidence_outbox');")
    check("postgres: FORCE RLS on evidence, versions, families and both outboxes", out == "t", out)
    rc, out, _ = psql("SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname='app_policy';")
    check("postgres: runtime role app_policy is neither superuser nor BYPASSRLS", out == "f", out)
    rc, out, _ = psql("SELECT category FROM decision_outcome_categories WHERE outcome='SIGNATURE_REQUIRED';", db="governance_decision_log")
    check("postgres: ledger categorises SIGNATURE_REQUIRED as ESCALATED (GDL 000018)", out == "ESCALATED", out)


# ── 2. access-control and authorization ──────────────────────────────────────

PDC05_ACTIONS = ["POLICY_TEST_MANAGE", "POLICY_TEST_RUN", "POLICY_WAIVER_REQUEST", "POLICY_WAIVER_APPROVE",
                 "POLICY_SIMULATE", "POLICY_EVIDENCE_REDELIVER"]


def grants():
    rc, out, _ = psql("SELECT string_agg(action_name, ',' ORDER BY action_name) FROM permission_definitions "
                      "WHERE action_name LIKE 'POLICY_%';", db="access_control")
    registered = set(out.split(",")) if out else set()
    need = {"POLICY_CREATE", "POLICY_READ", "POLICY_EVALUATE", "POLICY_VERSION_CREATE", "POLICY_VERSION_APPROVE",
            "POLICY_VERSION_ACTIVATE", "POLICY_VERSION_SUSPEND", "POLICY_VERSION_RETIRE", *PDC05_ACTIONS}
    check("access-control: every action policy-svc checks is registered (R5)", need <= registered, f"missing {sorted(need - registered)}")

    # DEVELOPMENT ONLY: the PDC-05 actions on the demo POLICY_FULL bundle (EST-9
    # grants them by template in production).
    acts = json.dumps(PDC05_ACTIONS)
    rc, _, err = psql(f"""UPDATE permission_bundles SET permitted_actions = (
        SELECT jsonb_agg(DISTINCT a) FROM jsonb_array_elements_text(permitted_actions || '{acts}'::jsonb) a)
        WHERE bundle_code = 'POLICY_FULL' AND active_flag;""", db="authorization_svc")
    check("authorization: PDC-05 actions on the demo POLICY_FULL bundle (dev grant)", rc == 0, err)
    time.sleep(6)  # authorization-svc decision cache

    for principal, scope, action in ((ADMIN, ENTITY, "POLICY_EVALUATE"), (APPROVER, ENTITY, "POLICY_VERSION_APPROVE"),
                                     (ADMIN, PLATFORM, "POLICY_TEST_MANAGE")):
        s, _, b, _ = call("POST", f"{AUTHZ}/v1/authorize", envelope(principal, entity=scope),
                          {"principal_id": principal, "legal_entity_id": scope, "action_type": action})
        check(f"authorization: {action} granted to {principal[:8]} on {scope[-4:]}", s == 200 and b.get("decision") == "PERMIT",
              f"HTTP {s} {b.get('decision')} {b.get('decision_basis', '')}")
    s, _, b, _ = resolve(resolve_body("5", actor_principal_id=STRANGER), principal=STRANGER)
    check("authorization: a principal without POLICY_EVALUATE cannot resolve (403)", s == 403, f"HTTP {s} {b.get('code')}")


# ── 3. identity-context-svc: the console's signed envelope ───────────────────

def envelope_jwt():
    s, _, b, _ = call("POST", f"{IDENTITY}/v1/authenticate", {"Content-Type": "application/json"},
                      {"tenant_id": TENANT, "email": "admin@zoikosuite.com", "password": "Zoiko@Governance1",
                       "client_id": os.environ.get("IDENTITY_CONSOLE_CLIENT_ID", "zoiko-console"),
                       "client_secret": os.environ.get("IDENTITY_CONSOLE_CLIENT_SECRET", "local-dev-console-client-secret")})
    if s != 200 or not b.get("access_token"):
        return None, f"login HTTP {s} {brief(b)}"
    s, _, r, _ = call("POST", f"{IDENTITY}/v1/context/resolve", envelope(write=True),
                      {"bearer_token": b["access_token"], "legal_entity_id": ENTITY})
    return (r.get("envelope_jwt"), "") if r.get("envelope_jwt") else (None, f"resolve HTTP {s} {brief(r)}")


def identity():
    jwt, why = envelope_jwt()
    if not check("identity: console envelope issued by identity-context-svc", jwt, why):
        return
    h = envelope()
    h["Authorization"] = "Bearer " + jwt
    s, hd, b, _ = call("GET", f"{POLICY}/v1/policy-types", h)
    check("identity: policy-svc accepts the verified envelope (200, not marked unverified)",
          s == 200 and hd.get("X-Identity-Assertion") != "missing", f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")
    head, payload, sig = jwt.split(".")
    h["Authorization"] = "Bearer " + ".".join([head, payload, sig[:-4] + ("AAAA" if not sig.endswith("AAAA") else "BBBB")])
    s, _, b, _ = call("GET", f"{POLICY}/v1/policy-types", h)
    check("identity: a tampered envelope is refused (401)", s == 401, f"HTTP {s} {b.get('code')}")


# ── 4. the family catalogue and P0-01 fail-closed ────────────────────────────

def catalogue():
    s, _, b, _ = pol("GET", "/v1/policy-types")
    cards = {i["policy_type"]: i for i in b.get("items", [])}
    check("catalogue: one status card per family, 8 outcomes, reason-code list versioned",
          s == 200 and len(b.get("outcomes", [])) == 8 and b.get("reason_codes_version") and
          {"APPROVAL_THRESHOLD", "SIGNATORY_MATRIX", "SPEND_CONTROL", "SOD_RULE", "LEGAL_BASIS"} <= set(cards), brief(b)[:200])
    for typ, owner in (("SPEND_CONTROL", "spend-controls-svc"), ("SOD_RULE", "authorization-svc"), ("LEGAL_BASIS", "privacy-decision-svc")):
        c = cards.get(typ, {})
        check(f"catalogue: {typ} is decided by {owner} (blocking, reference only)",
              c.get("owning_service") == owner and c.get("evaluation_mode") == "REFERENCE_ONLY" and c.get("blocking") is True
              and c.get("display_state") == "DECIDED_BY_OWNING_SERVICE", brief(c)[:200])
    for typ, owner in (("SPEND_CONTROL", "spend-controls-svc"), ("SOD_RULE", "authorization-svc"),
                       ("LEGAL_BASIS", "privacy-decision-svc"), ("SIGNATORY_MATRIX", "policy-svc"), (f"E2E_UNKNOWN_{RUN.upper()}", None)):
        s, _, b, _ = pol("POST", "/v1/policies/evaluate", write=False,
                         body={"policy_type": typ, "decision_id": f"e2e-ni-{RUN}-{typ[:6]}", "action_context": {"amount": 1}})
        check(f"P0-01: evaluate {typ} → 501 NOT_IMPLEMENTED, owner named, not retryable",
              s == 501 and b.get("outcome") == "NOT_IMPLEMENTED" and b.get("retryable") is False and
              (owner is None or b.get("owning_service") == owner), f"HTTP {s} {brief(b)[:160]}")


def reference_families(state):
    s, _, p, _ = pol("POST", "/v1/policies", body={"policy_code": f"E2E-SPEND-REF-{RUN}", "policy_name": "e2e spend reference",
                                                   "policy_type": "SPEND_CONTROL"})
    if not check("reference: SPEND_CONTROL family created", s in (200, 201), f"HTTP {s} {brief(p)}"):
        return
    vbase = {"effective_from": datetime.now(timezone.utc).isoformat(), "legal_entity_id": ENTITY}
    s, _, b, _ = pol("POST", f"/v1/policies/{p['policy_id']}/versions", body={**vbase, "rule_payload": {"limit_amount": "50000"}})
    check("reference: rule content in a spend version is refused (400; Spend Controls owns limits)",
          s == 400 and b.get("owning_service") == "spend-controls-svc", f"HTTP {s} {b.get('detail')}")
    s, _, b, _ = pol("POST", f"/v1/policies/{p['policy_id']}/versions",
                     body={**vbase, "rule_payload": {"rule_set_ref": "spend-controls:policy/MKT-UK", "rule_set_version": "2"}})
    check("reference: a governed reference to the spend rule set is accepted (201)", s == 201, f"HTTP {s} {brief(b)}")
    s, _, b, _ = pol("POST", "/v1/policies/evaluate", write=False,
                     body={"policy_type": "SPEND_CONTROL", "decision_id": f"e2e-ni2-{RUN}", "action_context": {"amount": 1}})
    check("P0-01: a configured spend reference still answers NOT_IMPLEMENTED (no fallback)", s == 501, f"HTTP {s}")


# ── 5. signatory matrix: authoring, maker-checker, certification, activation ──

def lifecycle(path, command, principal, version, extra=None):
    body = {"expected_version": version["row_version"]}
    body.update(extra or {})
    return pol("POST", f"{path}/{command}", principal=principal, body=body)


def release_matrix(state, code, matrix, label):
    """Draft, review (author), approve (checker), certify, activate (checker)."""
    s, _, fam, _ = pol("POST", "/v1/policies", body={"policy_code": code, "policy_name": f"e2e {label}",
                                                     "policy_type": "SIGNATORY_MATRIX"})
    if not check(f"{label}: family created", s in (200, 201), f"HTTP {s} {brief(fam)}"):
        return None
    s, _, v, _ = pol("POST", f"/v1/policies/{fam['policy_id']}/versions",
                     body={"effective_from": (datetime.now(timezone.utc) - timedelta(minutes=1)).isoformat(),
                           "legal_entity_id": ENTITY, "rule_payload": matrix, "rationale": "e2e fixture"})
    if not check(f"{label}: version drafted at the legal entity", s == 201, f"HTTP {s} {brief(v)}"):
        return None
    path = f"/v1/policies/{fam['policy_id']}/versions/{v['policy_version_id']}"
    s, _, v, _ = lifecycle(path, "submit-review", ADMIN, v)
    return fam, v, path


def certify(fam, v, cases):
    for code, ctx, expected in cases:
        s, _, b, _ = pol("POST", f"/v1/policies/{fam['policy_id']}/test-cases", body={
            "test_code": code, "layer": "BOUNDARY", "mandatory": True, "description": code,
            "action_context": ctx, "expected_result": expected})
        if s not in (200, 201):
            return False, f"case {code}: HTTP {s} {brief(b)}"
    s, _, run, _ = pol("POST", f"/v1/policy-versions/{v['policy_version_id']}/test-runs", body={})
    return s in (200, 201) and run.get("run_status") == "PASS", f"HTTP {s} {run.get('run_status')} failed={run.get('failed_count')} {brief(run.get('results', ''))[:160]}"


def retire_earlier_fixtures():
    """Earlier runs leave their e2e matrices ACTIVE; suspend them through the
    API (as the checker) so this run starts from no governing e2e matrix."""
    rc, out, _ = psql("SELECT p.policy_id, pv.policy_version_id, pv.row_version FROM policy_versions pv "
                      "JOIN policies p ON p.policy_id = pv.policy_id WHERE p.policy_code LIKE 'E2E-SIG-%' "
                      "AND pv.version_status IN ('ACTIVE', 'SCHEDULED');")
    n = 0
    for line in filter(None, out.splitlines()):
        pid, vid, rv = line.split("|")
        s, _, _, _ = pol("POST", f"/v1/policies/{pid}/versions/{vid}/suspend", principal=APPROVER,
                         body={"expected_version": int(rv), "reason_code": "E2E_CLEANUP", "reason": "earlier e2e run"})
        n += s == 200
    if out:
        check("setup: earlier runs' e2e matrices suspended", n == len(out.splitlines()), f"{n}/{len(out.splitlines())}")


def signatory(state):
    retire_earlier_fixtures()
    # Before any matrix: NOT_CONFIGURED, signed, recorded (P0-01).
    s, hd, d, raw = resolve(resolve_body("11000"))
    outcome, probs = contract_check(raw) if s == 200 else ("ERROR", [f"HTTP {s}"])
    if d.get("outcome") == "NOT_CONFIGURED":
        check("P0-01: no matrix → 200 NOT_CONFIGURED, CONFIG_POLICY_MISSING, signed and trusted",
              outcome == "NOT_CONFIGURED" and d.get("reason_codes") == ["CONFIG_POLICY_MISSING"] and not d.get("required_actions"),
              f"{outcome} {probs} {brief(d)[:160]}")
        state["not_configured"] = d
    else:
        skip("P0-01: no matrix → NOT_CONFIGURED", f"a SIGNATORY_MATRIX already governs this entity ({d.get('outcome')})")

    # Authoring: an overlapping matrix never reaches review.
    s, _, fam0, _ = pol("POST", "/v1/policies", body={"policy_code": f"E2E-SIG-BAD-{RUN}", "policy_name": "e2e bad",
                                                      "policy_type": "SIGNATORY_MATRIX"})
    bad = json.loads(json.dumps(MATRIX))
    bad["bands"][1]["above"] = "9000"
    s, _, b, _ = pol("POST", f"/v1/policies/{fam0.get('policy_id')}/versions",
                     body={"effective_from": datetime.now(timezone.utc).isoformat(), "legal_entity_id": ENTITY, "rule_payload": bad})
    check("authoring: overlapping bands refused at draft (400)", s == 400 and "overlap" in str(b.get("detail")), f"HTTP {s} {b.get('detail')}")

    got = release_matrix(state, f"E2E-SIG-{RUN}", MATRIX, "matrix")
    if not got:
        return
    fam, v, path = got
    s, _, b, _ = lifecycle(path, "approve", ADMIN, v)
    check("maker-checker: the author cannot approve their own matrix (403 SoD)", s == 403, f"HTTP {s} {b.get('code')}")
    s, _, v2, _ = lifecycle(path, "approve", APPROVER, v, {"approval_reference": f"e2e-approval-{RUN}"})
    if not check("maker-checker: an independent approver approves (200)", s == 200 and v2.get("version_status") == "APPROVED", f"HTTP {s} {brief(v2)}"):
        return
    v = v2
    s, _, b, _ = lifecycle(path, "activate", APPROVER, v)
    check("PDC-05: an uncertified matrix cannot go live (certification gate)", s in (409, 422) and "CERTIF" in json.dumps(b).upper(), f"HTTP {s} {b.get('code')} {b.get('reason_code')}")
    ok, why = certify(fam, v, [
        ("p0-05-11000", {"instrument_type": "SUPPLIER_CONTRACT", "amount": "11000", "currency": "GBP", "basis": "GROSS"}, "SIGNATURE_REQUIRED"),
        ("p0-05-100000.01", {"instrument_type": "SUPPLIER_CONTRACT", "amount": "100000.01", "currency": "GBP", "basis": "GROSS"}, "SIGNATURE_REQUIRED"),
        ("usd-no-fx", {"instrument_type": "SUPPLIER_CONTRACT", "amount": "1", "currency": "USD", "basis": "GROSS"}, "NOT_CONFIGURED"),
        ("nda-exempt", {"instrument_type": "NDA", "amount": "0", "currency": "GBP", "basis": "GROSS"}, "PERMIT"),
    ])
    check("PDC-05: the matrix's boundary suite runs through the live evaluator and passes", ok, why)
    s, _, v2, _ = lifecycle(path, "activate", APPROVER, v)
    if not check("lifecycle: certified matrix activated by the checker", s == 200 and v2.get("version_status") == "ACTIVE", f"HTTP {s} {brief(v2)}"):
        return
    state.update(matrix_family=fam, matrix_version=v2, matrix_path=path)

    # A second matrix for the same instrument at the same scope is ambiguous (§3.2).
    got = release_matrix(state, f"E2E-SIG-DUP-{RUN}", MATRIX, "duplicate matrix")
    if got:
        dfam, dv, dpath = got
        s, _, dv, _ = lifecycle(dpath, "approve", APPROVER, dv)
        certify(dfam, dv, [("x", {"instrument_type": "NDA", "amount": "0", "currency": "GBP", "basis": "GROSS"}, "PERMIT")])
        s, _, b, _ = lifecycle(dpath, "activate", APPROVER, dv)
        check("§3.2: a second matrix governing SUPPLIER_CONTRACT at the same scope is refused (409 POLICY_CONFLICTED)",
              s == 409 and b.get("code") == "POLICY_CONFLICTED", f"HTTP {s} {b.get('code')}")
    # The instrument-specific high-risk matrix coexists (Doc 01 §5.4).
    got = release_matrix(state, f"E2E-SIG-HR-{RUN}", HIGH_RISK_MATRIX, "high-risk matrix")
    if got:
        hfam, hv, hpath = got
        s, _, hv, _ = lifecycle(hpath, "approve", APPROVER, hv)
        certify(hfam, hv, [("g", {"instrument_type": "GUARANTEE", "amount": "1", "currency": "GBP", "basis": "GROSS"}, "SIGNATURE_REQUIRED")])
        s, _, hv2, _ = lifecycle(hpath, "activate", APPROVER, hv)
        check("§5.4: the high-risk matrix activates beside the ordinary one", s == 200 and hv2.get("version_status") == "ACTIVE", f"HTTP {s} {brief(hv2)[:160]}")
        state.update(hr_version=hv2, hr_path=hpath)


def resolutions(state):
    if not state.get("matrix_version"):
        skip("signatory resolutions", "no active matrix")
        return
    res = {"type": "contract", "id": f"C-{RUN}", "version": "3"}
    for amount, band, signers in (("11000", "B2", 2), ("100000", "B2", 2), ("100000.01", "B3", 2), ("10000.00", "B1", 1)):
        s, _, d, raw = resolve(resolve_body(amount))
        outcome, probs = contract_check(raw, resource=res) if s == 200 else ("ERROR", [f"HTTP {s} {brief(d)}"])
        q = (d.get("required_actions") or [{}])[0]
        check(f"P0-05: {amount} → SIGNATURE_REQUIRED band {band}, {signers} distinct signer(s), hash-bound, trusted",
              outcome == "SIGNATURE_REQUIRED" and d.get("rule_id", "").endswith("#band:" + band) and q.get("kind") == "SIGNATURE_QUORUM"
              and q.get("min_distinct_signers") == signers and q.get("document_hash") == DOC_HASH and "DISTINCT_HUMANS" in q.get("constraints", []),
              f"{outcome} {probs} {d.get('rule_id')} {brief(q)[:120]}")
        if amount == "11000":
            state["sig_decision"], state["sig_raw"], state["sig_body"] = d, raw, None
        if amount == "100000.01":
            kinds = [a.get("kind") for a in d.get("required_actions", [])]
            check("G-02: above 100,000 needs CFO then CEO and a separate release authorization",
                  q.get("order") == ["CFO", "CEO"] and "RELEASE_AUTHORIZATION" in kinds, brief(d.get("required_actions"))[:200])

    s, _, d, raw = resolve(resolve_body("0", instrument_type="NDA"))
    check("exemption: an NDA under the explicit exemption rule → PERMIT (SIGNATURE_EXEMPT)",
          d.get("outcome") == "PERMIT" and d.get("reason_codes", [""])[0] == "SIGNATURE_EXEMPT", brief(d)[:160])
    s, _, d, _ = resolve(resolve_body("5000", risk_flags=["NONSTANDARD_DATA_PROCESSING"]))
    check("G-01: nonstandard data-processing terms add mandatory legal review",
          "LEGAL_REVIEW" in [a.get("kind") for a in d.get("required_actions", [])] and "SIGNATURE_LEGAL_REVIEW_REQUIRED" in d.get("reason_codes", []), brief(d)[:160])
    s, _, d, _ = resolve(resolve_body("75000", money={"amount": "75000", "currency": "USD", "basis": "GROSS"}))
    check("FX: a USD amount with no owner valuation → NOT_CONFIGURED (no 1.0 default)",
          d.get("outcome") == "NOT_CONFIGURED" and d.get("reason_codes") == ["CONFIG_CURRENCY_UNSUPPORTED"], brief(d)[:160])
    s, _, d, _ = resolve(resolve_body("75000", money={"amount": "75000", "currency": "USD", "basis": "GROSS"},
                                      converted={"amount": "59000.00", "currency": "GBP", "fx_snapshot_ref": f"spend-fx-{RUN}"}))
    check("FX: Spend Controls' valuation is used and cited as an upstream reference",
          d.get("outcome") == "SIGNATURE_REQUIRED" and f"fx:spend-fx-{RUN}" in d.get("upstream_refs", []), brief(d)[:160])
    s, _, d, _ = resolve(resolve_body("9000", related_commitments=[{"resource_id": f"C-{RUN}-A1", "amount": "95000", "currency": "GBP"}]))
    check("§5.4: an amendment series is aggregated upward (9,000 + 95,000 → band B3)",
          d.get("rule_id", "").endswith("#band:B3") and "SIGNATURE_SERIES_AGGREGATED" in d.get("reason_codes", []), brief(d)[:160])
    s, _, d, _ = resolve(resolve_body("1", instrument_type="GUARANTEE"))
    if state.get("hr_version"):
        check("§5.4: a guarantee is governed by the high-risk matrix (two Directors + legal review)",
              d.get("outcome") == "SIGNATURE_REQUIRED" and d.get("policy_version_id") == state["hr_version"]["policy_version_id"]
              and "SIGNATURE_HIGH_RISK_INSTRUMENT" in d.get("reason_codes", []), brief(d)[:160])
    s, _, d, _ = resolve(resolve_body("1", instrument_type="CREDIT_FACILITY"))
    check("§5.4: a high-risk instrument no high-risk matrix lists → NOT_CONFIGURED (instrument matrix missing)",
          d.get("outcome") == "NOT_CONFIGURED" and d.get("reason_codes") == ["CONFIG_INSTRUMENT_MATRIX_MISSING"], brief(d)[:160])


def contract_negatives(state):
    if not state.get("sig_raw"):
        skip("contract negatives", "no signatory decision")
        return
    raw, d = state["sig_raw"], state["sig_decision"]
    # C02-P0-04/-09: replay and idempotency.
    body = resolve_body("11000", request_id=f"e2e-replay-{RUN}")
    s1, h1, d1, raw1 = resolve(body)
    s2, h2, d2, raw2 = resolve(body)
    check("C02-P0-09: the same request_id replays the same signed bytes (X-Decision-Replayed)",
          s1 == s2 == 200 and raw1 == raw2 and h2.get("X-Decision-Replayed") == "true", f"{s1}/{s2} replayed={h2.get('X-Decision-Replayed')}")
    body2 = dict(body, money={"amount": "11000.01", "currency": "GBP", "basis": "GROSS"})
    s, _, b, _ = resolve(body2)
    check("C02-P0-04: the same request_id with a changed amount → 409, never a merge", s == 409 and b.get("code") == "IDEMPOTENCY_MISMATCH", f"HTTP {s} {b.get('code')}")
    # C02-P0-03: scope comes from identity.
    s, _, b, _ = resolve(resolve_body("5", tenant_id=OTHER_TENANT))
    check("C02-P0-03: a spoofed tenant_id → 403", s == 403 and b.get("reason_code") == "tenant_scope_mismatch", f"HTTP {s} {b.get('reason_code')}")
    s, _, b, _ = resolve(resolve_body("5", legal_entity_id=OTHER_ENTITY))
    check("C02-P0-03: a legal entity other than the caller's → 403", s == 403 and b.get("reason_code") == "legal_entity_scope_mismatch", f"HTTP {s} {b.get('reason_code')}")
    s, _, b, _ = resolve(resolve_body("5", actor_principal_id=APPROVER))
    check("identity: actor_principal_id other than the verified caller → 403 (no impersonation)", s == 403 and b.get("reason_code") == "actor_mismatch", f"HTTP {s} {b.get('reason_code')}")
    raw_float = json.dumps(resolve_body("5")).replace('"amount": "5"', '"amount": 5').encode()
    s, _, b, _ = resolve(None, raw_body=raw_float)
    check("Doc 02 §2.1: a floating-point amount is a schema violation (400)", s == 400, f"HTTP {s} {b.get('reason_code')}")
    s, _, b, _ = resolve(resolve_body("5", auto_approve=True))
    check("Doc 02 §9: an unknown member is a schema violation (400), never ignored", s == 400 and b.get("reason_code") == "schema_violation", f"HTTP {s} {b.get('reason_code')}")
    # C02-P0-01/-13: the caller-side check turns anything untrustworthy into ERROR.
    tampered = raw.replace(b'"SIGNATURE_REQUIRED"', b'"PERMIT"', 1)
    out, probs = contract_check(tampered)
    check("C02-P0-13: a decision altered to PERMIT fails its signature → local ERROR", out == "ERROR" and "signature" in probs, f"{out} {probs}")
    unknown = json.loads(raw)
    unknown["outcome"] = "ALLOW"
    out, probs = contract_check(json.dumps(unknown).encode())
    check("C02-P0-01: an unknown outcome → local ERROR", out == "ERROR" and "unknown outcome" in probs, f"{out} {probs}")
    out, probs = contract_check(raw, resource={"type": "contract", "id": f"C-{RUN}", "version": "4"})
    check("C02-P0-04: a decision about another resource version → local ERROR", out == "ERROR" and "resource mismatch" in probs, f"{out} {probs}")
    out, probs = contract_check(raw, entity=OTHER_ENTITY)
    check("C02-P0-03: a decision for another legal entity → local ERROR", out == "ERROR", f"{out} {probs}")
    # The stored decision is the answered one, and the published key verifies it.
    s, _, _, got = pol("GET", f"/internal/v1/policy/decisions/{d['decision_id']}")
    check("decisions: GET /internal/v1/policy/decisions/{id} serves the exact signed bytes", s == 200 and got == raw, f"HTTP {s}")
    s, _, b, _ = pol("GET", f"/internal/v1/policy/decisions/{d['decision_id']}", tenant=OTHER_TENANT)
    check("isolation: another tenant cannot read the decision", s in (403, 404), f"HTTP {s} {b.get('code')}")
    rc, out, _ = psql(f"SELECT decision_outcome, valid_until IS NOT NULL, decision_envelope::jsonb ? 'integrity', policy_type "
                      f"FROM policy_evaluations WHERE evaluation_id = '{d['decision_id']}';")
    check("evidence: the decision is stored with outcome, validity and its signed envelope", out == "SIGNATURE_REQUIRED|t|t|SIGNATORY_MATRIX", out)


# ── 6. the governance ledger (as svc-policy) ─────────────────────────────────

def ledger(state):
    d = state.get("sig_decision")
    if not d:
        skip("ledger: signatory decision relayed", "no signatory decision")
        return

    def recorded():
        rc, out, _ = psql(f"SELECT o.delivered_at IS NOT NULL, o.failed_at IS NOT NULL, coalesce(o.last_error,'') FROM policy_evidence_outbox o "
                          f"WHERE o.evaluation_id = '{d['decision_id']}';")
        return (out.startswith("t|"), out)

    ok, out = until(recorded, 45, 2)
    check("ledger: the signatory decision's evidence is delivered by the relay", ok, out)
    rc, out, _ = psql(f"SELECT outcome, action_type, actor_id, coalesce(policy_version_id::text,''), "
                      f"(SELECT category FROM decision_outcome_categories c WHERE c.outcome = g.outcome) "
                      f"FROM governance_decisions g WHERE decision_id = '{d['request_id']}' AND tenant_id = '{TENANT}';",
                      db="governance_decision_log")
    check("ledger: recorded as svc-policy, outcome SIGNATURE_REQUIRED (ESCALATED), version pinned",
          out == f"SIGNATURE_REQUIRED|SIGNATORY_MATRIX|svc-policy|{d['policy_version_id']}|ESCALATED", out)
    nc = state.get("not_configured")
    if nc:
        until(lambda: (psql(f"SELECT 1 FROM governance_decisions WHERE decision_id = '{nc['request_id']}';", db="governance_decision_log")[1] == "1", None), 45, 2)
        rc, out, _ = psql(f"SELECT outcome, (SELECT category FROM decision_outcome_categories c WHERE c.outcome = g.outcome) "
                          f"FROM governance_decisions g WHERE decision_id = '{nc['request_id']}';", db="governance_decision_log")
        check("ledger: a NOT_CONFIGURED decision is recorded and categorised DENIED (blocked), never approved", out == "NOT_CONFIGURED|DENIED", out)


# ── 7. status card, metrics, suspension, Kafka, mTLS ─────────────────────────

def status_and_metrics(state):
    s, _, b, _ = pol("GET", "/v1/policy-types")
    card = next((i for i in b.get("items", []) if i["policy_type"] == "SIGNATORY_MATRIX"), {})
    gv = [g for g in card.get("status", {}).get("governing_versions", []) if state.get("matrix_version")
          and g["policy_version_id"] == state["matrix_version"]["policy_version_id"]]
    check("status card: SIGNATORY_MATRIX evaluable, governing version with approval and activation trail",
          card.get("display_state") == "EVALUABLE" and gv and gv[0].get("approved_by_principal_id") == APPROVER
          and gv[0].get("activated_by_principal_id") == APPROVER and gv[0].get("approval_reference") == f"e2e-approval-{RUN}",
          brief(gv)[:200])
    check("status card: the last day's decisions by outcome include SIGNATURE_REQUIRED",
          card.get("status", {}).get("decisions_24h", {}).get("SIGNATURE_REQUIRED", 0) >= 1, brief(card.get("status", {}).get("decisions_24h")))
    s, _, _, raw = call("GET", f"{POLICY}/metrics")
    text = raw.decode("utf8", "replace")
    check("metrics: policy_decisions_total counts SIGNATURE_REQUIRED and NOT_IMPLEMENTED hits",
          'outcome="SIGNATURE_REQUIRED",policy_type="SIGNATORY_MATRIX"' in text and 'outcome="NOT_IMPLEMENTED",policy_type="SPEND_CONTROL"' in text,
          "missing series" if 'policy_decisions_total' not in text else "")


def suspension(state):
    v, path = state.get("matrix_version"), state.get("matrix_path")
    if not v:
        skip("C02-P0-14: suspended matrix stops governing", "no active matrix")
        return
    s, _, v2, _ = lifecycle(path, "suspend", APPROVER, v, {"reason_code": "E2E_EMERGENCY", "reason": "e2e suspension"})
    if not check("lifecycle: emergency suspension of the matrix (200)", s == 200 and v2.get("version_status") == "SUSPENDED", f"HTTP {s} {brief(v2)[:160]}"):
        return
    time.sleep(1)
    s, _, d, raw = resolve(resolve_body("11000"))
    check("C02-P0-14: once suspended, the matrix no longer governs → NOT_CONFIGURED (no fallback)",
          d.get("outcome") == "NOT_CONFIGURED", brief(d)[:160])
    if state.get("hr_version"):
        s, _, _, _ = lifecycle(state["hr_path"], "suspend", APPROVER, state["hr_version"], {"reason_code": "E2E_CLEANUP"})
        check("cleanup: the high-risk fixture is suspended", s == 200, f"HTTP {s}")


def kafka(state):
    v = state.get("matrix_version")
    if not v:
        skip("kafka: policy.signatory_matrix.published", "no active matrix")
        return

    def published():
        rc, out, _ = psql(f"SELECT count(*) FILTER (WHERE published_at IS NOT NULL), count(*) FROM outbox "
                          f"WHERE event_type = 'policy.signatory_matrix.published' AND aggregate_id = '{state['matrix_family']['policy_id']}';")
        return (out.startswith("1|"), out)

    ok, out = until(published, 45, 2)
    check("outbox: policy.signatory_matrix.published written with the activation and relayed", ok, out)
    r = subprocess.run(["docker", "exec", KAFKA, "/opt/kafka/bin/kafka-console-consumer.sh", "--bootstrap-server", "localhost:9092",
                        "--topic", TOPIC, "--from-beginning", "--timeout-ms", "15000"], capture_output=True)
    lines = [l for l in r.stdout.decode("utf8", "replace").splitlines() if v["policy_version_id"] in l]
    types = set()
    for l in lines:
        try:
            types.add(json.loads(l).get("event_type"))
        except ValueError:
            pass
    check(f"kafka: policy.signatory_matrix.published and policy.version.activated on {TOPIC}",
          {"policy.signatory_matrix.published", "policy.version.activated"} <= types, f"{sorted(t for t in types if t)}")
    pub = next((json.loads(l) for l in lines if '"policy.signatory_matrix.published"' in l), None)
    if pub:
        payload = pub.get("payload") if isinstance(pub.get("payload"), dict) else json.loads(pub.get("payload") or "{}") \
            if isinstance(pub.get("payload"), str) else {}
        check("kafka: the event carries scope facts, never the bands",
              payload.get("currency") == "GBP" and "SUPPLIER_CONTRACT" in payload.get("instrument_types", []) and "bands" not in payload, brief(payload)[:200])


def provision_cert(service):
    r = subprocess.run(["docker", "exec", "mtls-management-svc", "cat", f"/bootstrap-tokens/{service}/token"], capture_output=True)
    token = r.stdout.decode().strip()
    if not token:
        return None, f"no bootstrap token for {service}"
    h = {"X-Tenant-Id": PLATFORM, "X-Legal-Entity-Id": PLATFORM, "X-Workload-Id": service,
         "X-Request-Id": str(uuid.uuid4()), "X-Correlation-ID": str(uuid.uuid4()), "X-Source-Channel": "system",
         "X-Purpose-Context": "service_identity_provisioning", "Idempotency-Key": str(uuid.uuid4()),
         "X-Mtls-Bootstrap-Token": token}
    s, _, b, _ = call("POST", f"{MTLS}/v1/mtls/certificates", h,
                      {"legal_entity_id": PLATFORM, "service_name": service, "common_name": service, "rotation_days": 1})
    if s != 201:
        return None, f"HTTP {s} {brief(b)}"
    d = os.path.join(tempfile.gettempdir(), f"policy-e2e-{service}-{RUN}")
    os.makedirs(d, exist_ok=True)
    paths = {}
    for name, pem in (("cert", b["certificate"]["certificate_pem"]), ("key", b["private_key_pem"]), ("ca", b["ca_certificate_pem"])):
        paths[name] = os.path.join(d, name + ".pem")
        with open(paths[name], "w", encoding="ascii") as f:
            f.write(pem)
    return paths, ""


def mtls_call(paths, method, path, headers):
    """One request to policy-svc's :8449 peer listener. It is published on no
    host port (host :8449 is authorization-svc), so the request is made from
    inside the compose network with the local alpine/openssl image. Returns
    (status, headers, body); status 0 when the TLS handshake was refused."""
    d = os.path.dirname(paths["ca"])
    req = f"{method} {path} HTTP/1.1\r\nHost: policy-svc\r\nConnection: close\r\n" + \
          "".join(f"{k}: {v}\r\n" for k, v in headers.items()) + "\r\n"
    args = ["docker", "run", "--rm", "-i", "--network", NETWORK, "-v", f"{d}:/pki:ro", "--entrypoint", "openssl",
            "alpine/openssl", "s_client", "-connect", "policy-svc:8449", "-CAfile", "/pki/ca.pem", "-quiet", "-verify_return_error"]
    if paths.get("cert"):
        args += ["-cert", "/pki/cert.pem", "-key", "/pki/key.pem"]
    r = subprocess.run(args, input=req.encode(), capture_output=True, timeout=60)
    out = r.stdout.decode("utf8", "replace")
    if not out.startswith("HTTP/"):
        return 0, {}, {"error": r.stderr.decode("utf8", "replace")[-200:]}
    head, _, body = out.partition("\r\n\r\n")
    lines = head.split("\r\n")
    hdrs = {k.strip(): v.strip() for k, _, v in (l.partition(":") for l in lines[1:])}
    if hdrs.get("Transfer-Encoding", "").lower() == "chunked":
        chunks, rest = [], body
        while rest:
            size, _, rest = rest.partition("\r\n")
            n = int(size or "0", 16)
            if n == 0:
                break
            chunks.append(rest[:n])
            rest = rest[n + 2:]
        body = "".join(chunks)
    try:
        parsed = json.loads(body) if body.strip() else {}
    except ValueError:
        parsed = {}
    return int(lines[0].split()[1]), hdrs, parsed


def mtls(state):
    # governance-decision-log-svc is an allowed peer (its replay reads policy
    # versions over :8449); notification-svc has no local bootstrap token.
    peer, why = provision_cert("governance-decision-log-svc")
    if not check("mtls: governance-decision-log-svc certificate from mtls-management-svc", peer, why):
        return
    s, _, b = mtls_call({"ca": peer["ca"]}, "GET", "/healthz", {})
    check("mtls: policy-svc :8449 refuses a client with no certificate", s == 0, f"status {s}")
    h = envelope(ADMIN)
    h["X-Source-Channel"] = "system"
    s, hd, b = mtls_call(peer, "GET", "/v1/policy-types", h)
    check("mtls: an allowed peer reaches policy-svc over :8449 and vouches for the principal (verified)",
          s == 200 and hd.get("X-Identity-Assertion") != "missing", f"HTTP {s} assertion={hd.get('X-Identity-Assertion')} {b.get('code', '')}")
    stranger, why = provision_cert("tenant-entity-registry-svc")
    if check("mtls: client certificate for a workload not allowed on :8449", stranger, why):
        s, _, b = mtls_call(stranger, "GET", "/v1/policy-types", h)
        check("mtls: a certificate not in MTLS_ALLOWED_CLIENTS is refused (403)", s == 403, f"HTTP {s}")


# ── 8. workflow-svc: the signature handoff (Doc 02 §4.2, §8.2) ───────────────

E2E_SIGNER_ROLES = {
    # role id: (code, actions, principal)
    "e2e00000-0000-4000-8000-00000000c501": ("E2E_SIGNER_DIRECTOR", ["WORKFLOW_APPROVE", "WORKFLOW_READ", "SIGNATORY_SIGN_AS_DIRECTOR"], SIGNER_DIRECTOR),
    "e2e00000-0000-4000-8000-00000000c502": ("E2E_SIGNER_FINANCE", ["SIGNATORY_SIGN_AS_FINANCE_SIGNER"], APPROVER),
    "e2e00000-0000-4000-8000-00000000c503": ("E2E_SIGNER_NO_OFFICE", ["WORKFLOW_APPROVE", "WORKFLOW_READ"], SIGNER_NO_OFFICE),
}


def grant_signers():
    """DEVELOPMENT ONLY: signer offices for the e2e principals at the legal
    entity. Production grants come from the entity's delegation of authority."""
    sql = []
    for role_id, (code, actions, principal) in E2E_SIGNER_ROLES.items():
        sql.append(f"""
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('{role_id}', '{TENANT}', '{code}', 'e2e: {code}', 'LEGAL_ENTITY', true, 'e2e') ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('{role_id}', '{code}', '{json.dumps(actions)}'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '{principal}', '{role_id}', '{ENTITY}'::uuid, now() - interval '1 minute', 'e2e'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments a WHERE a.principal_id = '{principal}' AND a.role_id = '{role_id}');""")
    rc, _, err = psql("\n".join(sql), db="authorization_svc")
    return rc == 0, err


def wf(method, path, principal=ADMIN, body=None, etag=None):
    h = envelope(principal, write=method != "GET")
    if etag:
        h["If-Match"] = etag
    s, hd, b, _ = call(method, WORKFLOW + path, h, body)
    return s, hd, b


def wf_etag(wid, principal=ADMIN):
    _, hd, _ = wf("GET", f"/v1/workflows/{wid}", principal)
    return hd.get("ETag") if hd else None


def signature_workflow(state):
    s, _, _, _ = call("GET", f"{WORKFLOW}/healthz", timeout=5)
    if not check("health workflow-svc", s == 200, f"HTTP {s}"):
        return
    if not state.get("matrix_version"):
        skip("workflow: signature handoff", "no active matrix")
        return
    ok, err = grant_signers()
    check("authorization: signer offices granted to the e2e signers (dev grant)", ok, err)
    time.sleep(6)
    s, _, b, _ = call("POST", f"{AUTHZ}/v1/authorize", envelope(SIGNER_DIRECTOR),
                      {"principal_id": SIGNER_DIRECTOR, "legal_entity_id": ENTITY, "action_type": "SIGNATORY_SIGN_AS_DIRECTOR"})
    check("authorization: SIGNATORY_SIGN_AS_DIRECTOR (access-control 000060) granted to the Director signer",
          s == 200 and b.get("decision") == "PERMIT", f"HTTP {s} {b.get('decision')} {b.get('decision_basis', '')}")

    s, _, d, _ = resolve(resolve_body("11000"))
    if not check("workflow: a SIGNATURE_REQUIRED decision to sign against (band B2: 2 signers, a Director)",
                 d.get("outcome") == "SIGNATURE_REQUIRED", brief(d)[:160]):
        return
    pool = [APPROVER, SIGNER_DIRECTOR, SIGNER_NO_OFFICE]
    base = {"tenant_id": TENANT, "legal_entity_id": ENTITY, "workflow_type": "CONTRACT_SIGNATURE",
            "signatory_decision_id": d["decision_id"],
            "stages": [{"stage_type": "QUORUM", "required_approvals": 2, "quorum_approvers": pool}]}

    nc = state.get("not_configured")
    if nc:
        s, _, b = wf("POST", "/v1/workflows", body=dict(base, signatory_decision_id=nc["decision_id"]))
        check("workflow: a NOT_CONFIGURED decision cannot start a signature workflow (422)",
              s == 422 and (b.get("code") or b.get("error")) == "signatory_decision_not_actionable", f"HTTP {s} {brief(b)[:160]}")
    s, _, b = wf("POST", "/v1/workflows", body=dict(base, stages=[{"stage_type": "QUORUM", "required_approvals": 1, "quorum_approvers": pool}]))
    check("workflow: a quorum smaller than the required distinct signers is refused (422)",
          s == 422 and (b.get("code") or b.get("error")) == "signature_quorum_insufficient", f"HTTP {s} {brief(b)[:160]}")
    s, _, b = wf("POST", "/v1/workflows", body=dict(base, subject_fingerprint="sha256:" + "f" * 64))
    check("workflow: a subject other than the decision's document is refused (409)",
          s == 409 and (b.get("code") or b.get("error")) == "signatory_subject_mismatch", f"HTTP {s} {brief(b)[:160]}")

    s, _, w = wf("POST", "/v1/workflows", body=base)
    if not check("workflow: signature workflow created from the verified decision (over policy-svc :8449)",
                 s == 201 and w.get("subject_fingerprint") == DOC_HASH and w.get("subject_id") == f"C-{RUN}" and w.get("subject_version") == 3,
                 f"HTTP {s} {brief(w)[:200]}"):
        return
    wid = w["workflow_instance_id"]
    s, _, g = wf("GET", f"/v1/workflows/{wid}")
    b = g.get("signatory_binding") or {}
    check("workflow: the binding pins the decision, its version and the document",
          b.get("signatory_decision_id") == d["decision_id"] and b.get("signatory_policy_version_id") == d["policy_version_id"]
          and b.get("document_hash") == DOC_HASH, brief(b)[:200])

    s, _, b = wf("POST", f"/v1/workflows/{wid}/quorum-votes", SIGNER_DIRECTOR,
                 {"action": "APPROVE", "document_hash": "sha256:" + "e" * 64}, wf_etag(wid))
    check("workflow: a signature on another document is refused (409 SIGNATURE_DOCUMENT_CHANGED)",
          s == 409 and b.get("reason_code") == "SIGNATURE_DOCUMENT_CHANGED", f"HTTP {s} {brief(b)[:160]}")
    s, _, b = wf("POST", f"/v1/workflows/{wid}/quorum-votes", SIGNER_NO_OFFICE,
                 {"action": "APPROVE", "document_hash": DOC_HASH}, wf_etag(wid))
    check("workflow: a signer authorization-svc confirms no office for cannot sign (403)",
          s == 403 and (b.get("code") or b.get("error")) == "signer_not_eligible", f"HTTP {s} {brief(b)[:160]}")
    s, _, b = wf("POST", f"/v1/workflows/{wid}/quorum-votes", APPROVER,
                 {"action": "APPROVE", "document_hash": DOC_HASH, "rationale": "e2e: finance signer"}, wf_etag(wid))
    check("workflow: the Finance signer signs (1 of 2, still PENDING)", s == 200 and b.get("workflow_status") in ("PENDING", None),
          f"HTTP {s} {brief(b)[:160]}")
    s, _, b = wf("POST", f"/v1/workflows/{wid}/quorum-votes", SIGNER_DIRECTOR,
                 {"action": "APPROVE", "document_hash": DOC_HASH, "rationale": "e2e: director"}, wf_etag(wid))
    s2, _, g = wf("GET", f"/v1/workflows/{wid}")
    check("workflow: the Director's signature fills the Director seat → APPROVED",
          s == 200 and g.get("workflow_status") == "APPROVED", f"HTTP {s} status={g.get('workflow_status')}")
    rc, out, _ = psql(f"SELECT string_agg(d.approver_principal_id || ':' || array_to_string(d.signer_offices, '+'), ',' ORDER BY d.acted_at) "
                      f"FROM workflow_stage_quorum_decisions d JOIN workflow_stages st ON st.workflow_stage_id = d.workflow_stage_id "
                      f"WHERE st.workflow_instance_id = '{wid}' AND d.decision = 'APPROVE' AND d.document_hash = '{DOC_HASH}';", db="workflow")
    check("workflow: each signature records the offices confirmed at signing and the document hash",
          out == f"{APPROVER}:FINANCE_SIGNER,{SIGNER_DIRECTOR}:DIRECTOR", out)

    rel = {"current_subject_fingerprint": DOC_HASH}
    s, _, b = wf("POST", f"/v1/workflows/{wid}/verify-release", APPROVER, rel, wf_etag(wid))
    check("workflow: release without a fresh signatory decision is refused (400)", s == 400 and b.get("field") == "current_signatory_decision_id",
          f"HTTP {s} {brief(b)[:160]}")
    s, _, fresh, _ = resolve(resolve_body("11000"))
    s, _, b = wf("POST", f"/v1/workflows/{wid}/verify-release", APPROVER, dict(rel, current_signatory_decision_id=fresh.get("decision_id")), wf_etag(wid))
    check("workflow: release re-check: the requirement in force now is met by the signatures given (200)",
          s == 200 and b.get("can_release") is True, f"HTTP {s} {brief(b)[:200]}")
    state["signature_workflow"] = wid


def release_after_suspension(state):
    wid = state.get("signature_workflow")
    if not wid:
        return
    s, _, fresh, _ = resolve(resolve_body("11000"))
    s, _, b = wf("POST", f"/v1/workflows/{wid}/verify-release", APPROVER,
                 {"current_subject_fingerprint": DOC_HASH, "current_signatory_decision_id": fresh.get("decision_id")}, wf_etag(wid))
    check("§8.4: once the matrix is suspended, release of the signed workflow is held (409, NOT_CONFIGURED now)",
          s == 409 and b.get("can_release") is False and "NOT_CONFIGURED" in str(b.get("reason")), f"HTTP {s} {brief(b)[:200]}")


def legacy_threshold():
    s, _, b, _ = pol("POST", "/v1/policies/evaluate", write=False,
                     body={"policy_type": "APPROVAL_THRESHOLD", "decision_id": f"e2e-at-{RUN}", "action_context": {"amount": 7500}})
    if s == 200:
        want = {"WITHIN_THRESHOLD": "PERMIT", "APPROVAL_REQUIRED": "APPROVAL_REQUIRED"}.get(b.get("result"))
        check("approval threshold: unchanged contract, now also carries the shared outcome",
              b.get("outcome") == want and b.get("owning_service") == "policy-svc" and b.get("contract_version") == "1.0", brief(b)[:200])
    else:
        check("approval threshold: no threshold governs → 404 with outcome NOT_CONFIGURED",
              s == 404 and b.get("outcome") == "NOT_CONFIGURED", f"HTTP {s} {brief(b)[:160]}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--no-kafka", action="store_true")
    ap.add_argument("--no-ledger", action="store_true")
    ap.add_argument("--no-mtls", action="store_true")
    ap.add_argument("--no-workflow", action="store_true")
    a = ap.parse_args()
    state = {}
    health()
    schema()
    grants()
    identity()
    catalogue()
    reference_families(state)
    signatory(state)
    resolutions(state)
    contract_negatives(state)
    legacy_threshold()
    if a.no_ledger:
        skip("ledger", "--no-ledger")
    else:
        ledger(state)
    status_and_metrics(state)
    if a.no_kafka:
        skip("kafka", "--no-kafka")
    else:
        kafka(state)
    if a.no_mtls:
        skip("mtls", "--no-mtls")
    else:
        mtls(state)
    if a.no_workflow:
        skip("workflow signature handoff", "--no-workflow")
    else:
        signature_workflow(state)
    suspension(state)
    if not a.no_workflow:
        release_after_suspension(state)
    passed = sum(1 for r in results if r[1] == "PASS")
    failed = [r for r in results if r[1] == "FAIL"]
    skipped = sum(1 for r in results if r[1] == "SKIP")
    print(f"\n{passed} passed, {len(failed)} failed, {skipped} skipped ({len(results)} checks, run {RUN})")
    for name, _, detail in failed:
        print(f"  FAIL {name}: {detail}")
    sys.exit(len(failed))


if __name__ == "__main__":
    main()
