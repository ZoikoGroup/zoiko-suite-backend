#!/usr/bin/env python3
"""Decision capture end to end (tracker "Assignment 25 Nov 2026", DL-9).

Every governance engine relays its own verdicts to governance-decision-log-svc
(Doc 01 s7). This drives one governed action per writer on the local stack and
checks that its decision lands in the ledger with the right actor (the
writer's service principal), outcome, rule_basis, on_behalf_of and
causation_id; then that a PERMIT never does (D-DL-1), that people cannot
record (D-DL-2), and that the chain verifies, seals and re-verifies.

    python deployments/scripts/e2e-decision-capture.py [--only authorization,jurisdiction,...]

Sections and what each needs up (besides the core: postgres, kafka, redis,
mtls-management, identity, tenant, access-control, authorization and
governance-svc):
  authorization  nothing more (personas: e2e-authorization-scenarios.py --seed-only)
  workflow       policy-svc and workflow-svc (DL-2)
  jurisdiction   jurisdiction-svc (DL-4)
  obligations    obligations-svc (DL-5)
  evidence       evidence-requirements-svc (DL-6)
  people         nothing more (D-DL-2)
  chain          nothing more
A section whose service does not answer is reported SKIP, not FAIL.
Exit code = number of failures.
"""
import argparse
import base64
import json
import sys
import time
import urllib.error
import urllib.request
import uuid

sys.stdout.reconfigure(encoding="utf-8")

H = "http://127.0.0.1"
GDL, AUTHZ, IDENTITY = f"{H}:8083", f"{H}:8089", f"{H}:8080"
POLICY, WORKFLOW, JURISDICTION = f"{H}:8085", f"{H}:8090", f"{H}:8082"
OBLIGATIONS, EVIDENCE = f"{H}:8088", f"{H}:8130"
TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
CLERK = "a1000000-0000-4000-8000-000000000002"
APPROVER = "a1000000-0000-4000-8000-000000000003"
RUN = uuid.uuid4().hex[:8]
WAIT = 30  # seconds a relayed decision may take to reach the ledger

LOGINS = {
    "admin": ("admin@zoikosuite.com", "Zoiko@Governance1"),
    "legal": ("legal.counsel@zoikosuite.com", "Zoiko@Legal2026!"),
    "cfo": ("cfo@zoikosuite.com", "Zoiko@Finance2026!"),
    "hr": ("hr.director@zoikosuite.com", "Zoiko@People2026!"),
}

results = {"pass": 0, "fail": 0, "skip": 0}


def check(name, ok, detail=""):
    results["pass" if ok else "fail"] += 1
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  - {detail}" if detail and not ok else ""), flush=True)
    return bool(ok)


def skip(name, why):
    results["skip"] += 1
    print(f"SKIP  {name}  - {why}", flush=True)


def call(method, url, headers=None, body=None, timeout=20):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            status = r.status
    except urllib.error.HTTPError as e:
        raw, status = e.read(), e.code
    except (urllib.error.URLError, OSError) as e:
        return 0, {"error": f"unreachable: {e}"}
    try:
        return status, (json.loads(raw) if raw else {})
    except ValueError:
        return status, {"raw": raw[:200].decode("utf8", "replace")}


def brief(b):
    return json.dumps(b)[:240]


def headers(principal=ADMIN, write=False, purpose="GOVERNANCE_AUDIT", entity=ENTITY, extra=None):
    rid = str(uuid.uuid4())
    h = {"X-Tenant-Id": TENANT, "X-Principal-Id": principal, "X-Legal-Entity-Id": entity,
         "X-Correlation-ID": f"e2e-dc-{RUN}-{rid[:8]}", "X-Request-Id": rid, "X-Source-Channel": "api",
         "X-Source-System": "e2e-decision-capture"}
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
    else:
        h["X-Purpose-Context"] = purpose
    h.update(extra or {})
    return h


_SESSIONS = {}


def session(who):
    """(envelope_jwt, principal_id) of a demo login, renewed every 200 s."""
    jwt, principal, at = _SESSIONS.get(who, (None, None, 0))
    if jwt and time.time() - at < 200:
        return jwt, principal
    email, password = LOGINS[who]
    s, b = call("POST", f"{IDENTITY}/v1/authenticate", {},
                {"tenant_id": TENANT, "email": email, "password": password,
                 "client_id": "zoiko-console", "client_secret": "local-dev-console-client-secret"})
    if s != 200 or not b.get("access_token"):
        raise RuntimeError(f"login {email}: HTTP {s} {brief(b)}")
    s, r = call("POST", f"{IDENTITY}/v1/context/resolve", headers(write=True),
                {"bearer_token": b["access_token"], "legal_entity_id": ENTITY})
    if s != 200 or not r.get("envelope_jwt"):
        raise RuntimeError(f"resolve {email}: HTTP {s} {brief(r)}")
    # The envelope names its principal in its claims (principal.principal_id).
    claims = r["envelope_jwt"].split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(claims + "=" * (-len(claims) % 4)))
    principal = (claims.get("principal") or {}).get("principal_id")
    if not principal:
        raise RuntimeError(f"resolve {email}: the envelope names no principal")
    _SESSIONS[who] = (r["envelope_jwt"], principal, time.time())
    return r["envelope_jwt"], principal


def as_person(who, write=False, purpose="GOVERNANCE_AUDIT", extra=None):
    jwt, principal = session(who)
    h = headers(principal, write=write, purpose=purpose, extra=extra)
    h["Authorization"] = "Bearer " + jwt
    return h


def ledger_decision(decision_id):
    s, b = call("GET", f"{GDL}/v1/decisions/{decision_id}", as_person("admin"))
    return (b if s == 200 else None), s


def until_in_ledger(decision_id, seconds=WAIT):
    end = time.time() + seconds
    status = 0
    while time.time() < end:
        d, status = ledger_decision(decision_id)
        if d:
            return d
        time.sleep(1)
    return None


def up(url):
    s, _ = call("GET", url + "/healthz", timeout=5)
    return s == 200


# ── authorization-svc (DL-3, D-DL-1) ───────────────────────────────────────

def authorization():
    def decide(principal, action, attributes=None):
        body = {"principal_id": principal, "legal_entity_id": ENTITY, "action_type": action, "tenant_id": TENANT}
        if attributes:
            body["attributes"] = attributes
        return call("POST", f"{AUTHZ}/v1/authorize", headers(principal), body)

    s, deny = decide(CLERK, "zt.payment.release")
    if not check("authorization: clerk releasing a payment is DENY", s == 200 and deny.get("decision") == "DENY",
                 f"HTTP {s} {brief(deny)}"):
        return
    s, step = decide(APPROVER, "zt.period.reopen", {"authn_age_seconds": "3600"})
    check("authorization: reopen with an old login is STEP_UP", s == 200 and step.get("decision") == "STEP_UP",
          f"HTTP {s} {brief(step)}")
    s, permit = decide(APPROVER, "zt.period.reopen", {"authn_age_seconds": "60"})
    check("authorization: reopen with a fresh login is PERMIT", s == 200 and permit.get("decision") == "PERMIT",
          f"HTTP {s} {brief(permit)}")

    t0 = time.time()
    d = until_in_ledger(deny["access_decision_id"])
    if check(f"authorization: the DENY reaches the ledger within {WAIT} s", d,
             f"{deny['access_decision_id']} not in the ledger"):
        ec = d.get("evaluation_context") or {}
        check("authorization: ledger actor is svc-authorization", d.get("actor_id") == "svc-authorization", d.get("actor_id"))
        check("authorization: outcome DENY (category DENIED), action ACCESS_zt.payment.release",
              d.get("outcome") == "DENY" and d.get("action_type") == "ACCESS_zt.payment.release",
              f"{d.get('outcome')} {d.get('action_type')}")
        check("authorization: on_behalf_of is the original principal", ec.get("on_behalf_of") == CLERK, ec)
        check("authorization: rule_basis names the policy-set version and reasons",
              d.get("rule_basis", "").startswith("cfg.") and "basis:" in d.get("rule_basis", ""), d.get("rule_basis"))
        check("authorization: no attribute values reach the ledger", "attributes" not in ec, list(ec))
        print(f"      (DENY in the ledger after {time.time() - t0:.1f} s)")
    # D-DL-1: give the relay the same time, then neither the STEP_UP nor the PERMIT may be there.
    time.sleep(3)
    for name, dec in (("STEP_UP", step), ("PERMIT", permit)):
        if dec.get("access_decision_id"):
            got, st = ledger_decision(dec["access_decision_id"])
            check(f"authorization: a {name} is not relayed (D-DL-1)", got is None and st == 404, f"HTTP {st}")


# ── evidence-requirements-svc (DL-6) ───────────────────────────────────────

def evidence():
    action = f"DC_E2E_{RUN.upper()}"
    s, b = call("POST", f"{EVIDENCE}/v1/evidence/evaluate", as_person("admin", write=True),
                {"legal_entity_id": ENTITY, "domain_code": "E2E", "action_type": action,
                 "correlation_id": f"e2e-dc-ev-{RUN}", "present_artifacts": []})
    if not check("evidence: an evaluation is answered", s in (200, 201) and b.get("evaluation_id"), f"HTTP {s} {brief(b)}"):
        return
    d = until_in_ledger(b["evaluation_id"])
    if check(f"evidence: the evaluation reaches the ledger within {WAIT} s", d, b["evaluation_id"]):
        check("evidence: recorded by svc-evidence-requirements with the evaluated outcome",
              d.get("actor_id") == "svc-evidence-requirements" and d.get("outcome") == b.get("outcome")
              and d.get("action_type") == "EVIDENCE_" + action, f"{d.get('actor_id')} {d.get('outcome')} {d.get('action_type')}")
        check("evidence: rule_basis is the requirement-set version",
              d.get("rule_basis", "").startswith(f"requirements:E2E/{action}@sha256:"), d.get("rule_basis"))
        check("evidence: status category is never OTHER", d.get("status_category") in ("APPROVED", "DENIED"), d.get("status_category"))


# ── policy-svc + workflow-svc (DL-2) ───────────────────────────────────────

def workflow():
    decision_id = f"e2e-dc-pol-{RUN}"
    s, ev = call("POST", f"{POLICY}/v1/policies/evaluate", as_person("admin", purpose="POLICY_REVIEW"),
                 {"policy_type": "APPROVAL_THRESHOLD", "tenant_id": TENANT, "legal_entity_id": ENTITY,
                  "action_context": {"amount": 50_000_000}, "decision_id": decision_id})
    if not check("workflow: policy evaluates a payment above its threshold", s in (200, 201), f"HTTP {s} {brief(ev)}"):
        return
    pd = until_in_ledger(decision_id, 60)
    if not check("workflow: the policy decision is in the ledger, ESCALATED, by svc-policy",
                 pd and pd.get("actor_id") == "svc-policy" and pd.get("status_category") == "ESCALATED",
                 brief(pd) if pd else "not in the ledger"):
        return
    _, approver = session("legal")
    s, wf = call("POST", f"{WORKFLOW}/v1/workflows", as_person("admin", write=True, extra={"X-Causation-Id": decision_id}),
                 {"tenant_id": TENANT, "legal_entity_id": ENTITY, "workflow_type": "PAYMENT_APPROVAL",
                  "stages": [{"approver_principal_id": approver}]})
    # The instance fields are at the top level of the response (the stages
    # sit beside them), not under a "workflow_instance" key.
    inst = wf if isinstance(wf, dict) else {}
    if not check("workflow: started for the escalated decision, which it stores", s == 201 and inst.get("causation_decision_id") == decision_id,
                 f"HTTP {s} {brief(wf)}"):
        return
    s, act = call("POST", f"{WORKFLOW}/v1/workflows/{inst['workflow_instance_id']}/actions",
                  as_person("legal", write=True, extra={"If-Match": f'"v{inst.get("row_version", 1)}"'}),
                  {"action": "APPROVE", "rationale": "e2e decision capture"})
    check("workflow: the approver approves", s == 200, f"HTTP {s} {brief(act)}")
    end, found = time.time() + 60, None
    while time.time() < end and not found:
        s, page = call("GET", f"{GDL}/v1/decisions?causation={decision_id}", as_person("admin"))
        found = next((r for r in (page.get("records") or []) if r.get("actor_id") == "svc-workflow"), None) if s == 200 else None
        time.sleep(1 if not found else 0)
    check("workflow: the approval is in the ledger with causation_id = the policy decision", found, "no svc-workflow follow-up")


# ── jurisdiction-rules-svc (DL-4) ──────────────────────────────────────────

def jurisdiction():
    """A payment term drafted by the admin and approved by legal counsel (maker-checker)
    is a governed decision: ACTIVATED, recorded by svc-jurisdiction-rules. The pack
    certify / publish path is driven by e2e-jurisdiction-rules-deps.py's ledger section."""
    term = f"DC{RUN.upper()}"[:32]
    s, b = call("POST", f"{JURISDICTION}/v1/admin/payment-terms", as_person("admin", write=True),
                {"term_code": term, "term_name": f"Decision capture e2e {RUN}"})
    check("jurisdiction: the admin registers a payment term", s in (200, 201), f"HTTP {s} {brief(b)}")
    s, v = call("POST", f"{JURISDICTION}/v1/admin/payment-terms/{term}/versions", as_person("admin", write=True),
                {"basis_date_rule": "INVOICE_DATE", "due_method": "NET_DAYS", "due_days": 30,
                 "business_day_convention": "NONE", "rationale": "decision capture e2e", "effective_from": "2026-01-01"})
    if not check("jurisdiction: the admin drafts version 1", s == 201, f"HTTP {s} {brief(v)}"):
        return
    s, a = call("POST", f"{JURISDICTION}/v1/admin/payment-terms/{term}/versions/1/approve", as_person("legal", write=True), {})
    if not check("jurisdiction: legal counsel approves it (a second person)", s == 200, f"HTTP {s} {brief(a)}"):
        return
    _, legal = session("legal")
    end, found = time.time() + WAIT, None
    while time.time() < end and not found:
        s, page = call("GET", f"{GDL}/v1/decisions?action=JURISDICTION_PAYMENT_TERM_APPROVE&rule_basis=payment_term:{term}@1",
                       as_person("admin"))
        found = (page.get("records") or [None])[0] if s == 200 else None
        if not found:
            time.sleep(1)
    if check(f"jurisdiction: the approval reaches the ledger within {WAIT} s", found, "not found"):
        ec = found.get("evaluation_context") or {}
        check("jurisdiction: recorded by svc-jurisdiction-rules, ACTIVATED, on behalf of the approver",
              found.get("actor_id") == "svc-jurisdiction-rules" and found.get("outcome") == "ACTIVATED" and ec.get("on_behalf_of") == legal,
              f"{found.get('actor_id')} {found.get('outcome')} {ec.get('on_behalf_of')}")
        check("jurisdiction: filed under the ledger's PLATFORM scope (reference data)", found.get("legal_entity_id") == "PLATFORM",
              found.get("legal_entity_id"))


# ── obligations-svc (DL-5) ─────────────────────────────────────────────────

def obligations():
    skip("obligations: evaluate and waive reach the ledger",
         "POST /v1/obligations/evaluate needs a published obligation rule, calendar and definition chain the local "
         "seed does not have; the record builders are unit-tested (TestEvaluateRecord_*, TestWaiveRecord_*)")


# ── people never record (D-DL-2) ────────────────────────────────────────────

def people():
    for who in ("admin", "hr"):
        corr = f"e2e-dc-person-{RUN}"
        s, b = call("POST", f"{GDL}/v1/decisions", as_person(who, write=True, extra={"X-Correlation-ID": corr}),
                    {"decision_id": f"e2e-dc-person-{who}-{RUN}", "legal_entity_id": ENTITY, "action_type": "PERSON_RECORD_PROBE",
                     "outcome": "APPROVED", "rule_basis": "e2e", "correlation_id": corr})
        check(f"people: {who}@ cannot record a decision (D-DL-2) -> 403", s == 403, f"HTTP {s} {brief(b)}")


# ── chain, seal, package verify ─────────────────────────────────────────────

def chain():
    s, v = call("GET", f"{GDL}/v1/evidence/chain/verify", as_person("admin"), timeout=60)
    check("chain: the whole ledger re-verifies", s == 200 and v.get("valid") is True, f"HTTP {s} {brief(v)}")
    s, p = call("POST", f"{GDL}/v1/evidence/packages/seal", headers("svc-governance-decision-log", write=True), {})
    if s in (200, 201) and p.get("package_id"):
        pkg = p["package_id"]
    else:
        # People do not seal (D-DL-2): the sealer does, hourly. Verify the newest package.
        s, lst = call("GET", f"{GDL}/v1/evidence/packages?limit=1", as_person("admin"))
        items = lst.get("packages") or lst.get("records") or (lst if isinstance(lst, list) else [])
        pkg = items[0]["package_id"] if items else None
    if pkg:
        s, pv = call("GET", f"{GDL}/v1/evidence/packages/{pkg}/verify", as_person("admin"))
        check("chain: the newest sealed package re-verifies", s == 200 and pv.get("valid") is True, f"HTTP {s} {brief(pv)}")
    else:
        skip("chain: package verify", "no sealed package yet (the sealer runs hourly)")


SECTIONS = {
    "authorization": (authorization, None),
    "workflow": (workflow, [POLICY, WORKFLOW]),
    "evidence": (evidence, [EVIDENCE]),
    "jurisdiction": (jurisdiction, [JURISDICTION]),
    "obligations": (obligations, [OBLIGATIONS]),
    "people": (people, None),
    "chain": (chain, None),
}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="")
    a = ap.parse_args()
    only = {x.strip() for x in a.only.split(",") if x.strip()}
    if not up(GDL) or not up(AUTHZ):
        print("governance-decision-log-svc and authorization-svc must be up")
        return 1
    for name, (fn, needs) in SECTIONS.items():
        if only and name not in only:
            continue
        print(f"-- {name} --", flush=True)
        if needs and not all(up(u) for u in needs):
            skip(name, "service not up: " + ", ".join(u for u in needs if not up(u)))
            continue
        try:
            fn()
        except Exception as e:  # noqa: BLE001 - a section's crash is one failure, not the run's
            check(f"{name}: section ran", False, repr(e))
    print(f"\n{results['pass']} pass / {results['fail']} fail / {results['skip']} skip")
    return results["fail"]


if __name__ == "__main__":
    sys.exit(main())
