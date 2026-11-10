#!/usr/bin/env python3
"""Persona scenarios for authorization-svc against the local Docker stack.

Seeds a small, realistic set of users on the demo tenant (idempotently, through
authorization-svc's own admin API) and then drives every layer of the decision
pipeline and every governed admin lifecycle with them, asserting the decision,
the deciding layer and the evidence.

    python deployments/scripts/e2e-authorization-scenarios.py [--seed-only]

Prerequisites: seed-demo-rbac.ps1 and seed-demo-registry.ps1 (the demo admin
33333333-... must hold the iam.* administration powers, and the demo entity
22222222-... must be registered). The stack needs postgres, kafka,
mtls-management-svc, identity-svc, tenant-entity-registry-svc and
authorization-svc; this script talks to authorization-svc only, on the
loopback plaintext port, where AUTHZ_IDENTITY_ASSERTION=warn admits the
X-Principal-Id header (local only).

Personas (tenant 11111111-..., entity 22222222-...):

    admin    33333333-...-333333333333  demo console admin (maker of every change)
    checker  a1000000-...-000000000001  Priya — IAM checker: approves limits and rules,
                                         suspends and resumes assignments
    clerk    a1000000-...-000000000002  Ravi — AP clerk: prepares payments
    approver a1000000-...-000000000003  Meera — AP approver: approves and releases,
                                         limit 10,000 GBP and 50 units
    support  a1000000-...-000000000004  Sam — vendor support operator
    leaver   a1000000-...-000000000005  Kiran — views invoices; suspended mid-run
    workload zt-invoice-batch-worker    a service identity bound to zt.invoice.view

Exit status is the number of failed checks.
"""
import argparse, json, sys, time, urllib.error, urllib.parse, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")

AUTHZ = "http://127.0.0.1:8089"
TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
CHECKER = "a1000000-0000-4000-8000-000000000001"
CLERK = "a1000000-0000-4000-8000-000000000002"
APPROVER = "a1000000-0000-4000-8000-000000000003"
SUPPORT = "a1000000-0000-4000-8000-000000000004"
LEAVER = "a1000000-0000-4000-8000-000000000005"
WORKLOAD = "zt-invoice-batch-worker"

PREPARE, APPROVE, RELEASE, VIEW, EXPORT, REOPEN = (
    "zt.payment.prepare", "zt.payment.approve", "zt.payment.release",
    "zt.invoice.view", "zt.invoice.export", "zt.period.reopen")

ROLES = {
    "ZT_AP_CLERK": [PREPARE, VIEW],
    "ZT_AP_APPROVER": [APPROVE, RELEASE, VIEW, REOPEN],
    "ZT_IAM_CHECKER": ["iam.authority_limit.approve", "iam.sod_rule.publish", "iam.policy.publish",
                       "iam.assignment.suspend", "iam.assignment.resume", "iam.assignment.read",
                       "iam.role.read", "iam.policy.read"],
    "ZT_INVOICE_VIEWER": [VIEW, EXPORT],
    "ZT_BATCH_WORKER": [VIEW, PREPARE],
}
GRANTS = [(CHECKER, "ZT_IAM_CHECKER"), (CLERK, "ZT_AP_CLERK"), (APPROVER, "ZT_AP_APPROVER"),
          (LEAVER, "ZT_INVOICE_VIEWER"), (WORKLOAD, "ZT_BATCH_WORKER")]

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail and not ok else ""), flush=True)
    return bool(ok)


def headers(principal, write=False, if_match=None, key=None):
    rid = str(uuid.uuid4())
    h = {"X-Tenant-Id": TENANT, "X-Principal-Id": principal, "X-Legal-Entity-Id": ENTITY,
         "X-Correlation-ID": "zt-" + rid[:8], "X-Request-Id": rid, "X-Source-Channel": "api",
         "X-Source-System": "e2e-authorization-scenarios"}
    if write:
        h["Idempotency-Key"] = key or str(uuid.uuid4())
        h["X-Occurred-At"] = datetime.now(timezone.utc).isoformat()
    if if_match:
        h["If-Match"] = if_match
    return h


def call(method, path, principal=ADMIN, body=None, write=None, if_match=None, key=None, timeout=15):
    if write is None:
        write = method != "GET"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(AUTHZ + path, data=data, method=method,
                                 headers=headers(principal, write, if_match, key))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
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


def brief(b):
    return json.dumps(b)[:260]


def etag(v):
    return f'"v{v}"'


def authorize(principal, action, attributes=None, **extra):
    body = {"principal_id": principal, "legal_entity_id": ENTITY, "action_type": action, "tenant_id": TENANT}
    if attributes:
        body["attributes"] = attributes
    body.update(extra)
    s, _, b = call("POST", "/v1/authorize", principal=ADMIN, body=body, write=False)
    return s, b


def expect(name, principal, action, decision, basis_prefix=None, attributes=None, codes=(), **extra):
    s, b = authorize(principal, action, attributes, **extra)
    basis = b.get("decision_basis", "")
    ok = s == 200 and b.get("decision") == decision and (basis_prefix is None or basis.startswith(basis_prefix))
    ok = ok and all(c in (b.get("reason_codes") or []) for c in codes)
    check(name, ok, f"HTTP {s} decision={b.get('decision')} basis={basis} codes={b.get('reason_codes')} {b.get('error', '')}")
    return b


# ── seeding (idempotent) ─────────────────────────────────────────────────────

def ensure_role(code, actions):
    s, _, b = call("POST", "/v1/admin/roles", body={"tenant_id": TENANT, "role_code": code,
                   "role_name": code.replace("_", " ").title(), "role_scope_type": "TENANT",
                   "created_by_principal_id": ADMIN})
    if s not in (200, 201) or not b.get("role_id"):
        check(f"seed: role {code}", False, f"HTTP {s} {brief(b)}")
        return None
    rid = b["role_id"]
    s, _, bl = call("GET", f"/v1/admin/roles/{rid}/permission-bundles")
    current = next((x for x in (bl if isinstance(bl, list) else []) if x.get("bundle_code") == "MAIN" and x.get("active_flag")), None)
    if current and sorted(current.get("permitted_actions") or []) == sorted(actions):
        return rid
    s, _, bb = call("POST", f"/v1/admin/roles/{rid}/permission-bundles",
                    body={"bundle_code": "MAIN", "permitted_actions": actions, "reason_code": "PERSONA_SEED",
                          "purpose": "authorization persona scenarios",
                          **({"expected_version": current["version"]} if current else {})})
    check(f"seed: bundle on {code}", s in (200, 201), f"HTTP {s} {brief(bb)}")
    return rid


def ensure_assignment(principal, role_id, code, entity=ENTITY):
    s, _, b = call("GET", f"/v1/admin/role-assignments?principal_id={urllib.parse.quote(principal)}&role_id={role_id}")
    rows = b if isinstance(b, list) else []
    live = [a for a in rows if a.get("approval_status") in ("APPROVED", "SUSPENDED")
            and (a.get("legal_entity_id") or "") == entity and not a.get("effective_to")]
    for a in live:
        if a.get("approval_status") == "SUSPENDED":
            # A previous run stopped between suspend and resume: lift it.
            call("POST", f"/v1/admin/role-assignments/{a['principal_role_assignment_id']}/resume", principal=CHECKER,
                 body={"reason_code": "PERSONA_SEED"}, if_match=etag(a["version"]))
        return a["principal_role_assignment_id"]
    s, _, b = call("POST", "/v1/admin/role-assignments", body={
        "principal_id": principal, "role_id": role_id, "legal_entity_id": entity,
        "effective_from": (datetime.now(timezone.utc) - timedelta(minutes=1)).isoformat(),
        "approval_reference": "ZT-PERSONA-SEED"})
    if not check(f"seed: {code} granted to {principal[:13]}", s in (200, 201) and b.get("approval_status") == "APPROVED",
                 f"HTTP {s} {brief(b)}"):
        return None
    return b["principal_role_assignment_id"]


def approve_rule(kind, rule):
    rid = rule.get("sod_rule_id") or rule.get("abac_rule_id")
    s, _, b = call("POST", f"/v1/admin/{kind}/{rid}/approve", principal=CHECKER,
                   body={"reason_code": "PERSONA_SEED", "purpose": "independent publication"}, if_match=etag(rule["version"]))
    return check(f"seed: {kind} {rid[:8]} published by the checker", s == 200, f"HTTP {s} {brief(b)}")


def ensure_sod_rule():
    s, _, b = call("GET", "/v1/admin/sod-rules")
    rules = b if isinstance(b, list) else []
    r = next((x for x in rules if {x.get("action_a"), x.get("action_b")} == {PREPARE, RELEASE} and x.get("tenant_id") == TENANT), None)
    if r is None:
        s, _, r = call("POST", "/v1/admin/sod-rules", body={"domain_code": "AP", "action_a": PREPARE, "action_b": RELEASE,
                       "conflict_type": "STATIC_HIGH", "tenant_id": TENANT, "reason_code": "PERSONA_SEED",
                       "purpose": "preparer must not release"})
        if not check("seed: SoD rule PREPARE x RELEASE requested", s in (201, 202), f"HTTP {s} {brief(r)}"):
            return
    if r.get("publication_status") == "PENDING_APPROVAL" or r.get("pending_change"):
        approve_rule("sod-rules", r)


def ensure_abac_rule():
    s, _, b = call("GET", "/v1/admin/abac-rules")
    rules = b if isinstance(b, list) else []
    r = next((x for x in rules if x.get("rule_code") == "ZT_NO_RESTRICTED_RELEASE"), None)
    if r is None:
        s, _, r = call("POST", "/v1/admin/abac-rules", body={"rule_code": "ZT_NO_RESTRICTED_RELEASE", "action_type": RELEASE,
                       "effect": "FORBID", "attribute_key": "classification", "operator": "eq",
                       "attribute_value": "restricted", "tenant_id": TENANT, "reason_code": "PERSONA_SEED",
                       "purpose": "restricted payments are released outside the AP team"})
        if not check("seed: ABAC rule requested", s in (201, 202), f"HTTP {s} {brief(r)}"):
            return
    if r.get("publication_status") == "PENDING_APPROVAL" or r.get("pending_change"):
        approve_rule("abac-rules", r)


def ensure_limit(kind, upper, currency=None):
    s, _, b = call("GET", f"/v1/admin/authority-limits?principal_id={APPROVER}")
    limits = (b or {}).get("authority_limits") or []
    lim = next((x for x in limits if x.get("authority_type") == RELEASE and x.get("limit_kind", "MONETARY") == kind
                and x.get("status") in ("PENDING_APPROVAL", "ACTIVE")), None)
    if lim is None:
        body = {"principal_id": APPROVER, "authority_type": RELEASE, "legal_entity_id": ENTITY, "limit_kind": kind,
                "lower_limit": "0", "upper_limit": upper, "reason_code": "PERSONA_SEED", "purpose": "AP approver authority"}
        if currency:
            body["currency"] = currency
        s, _, lim = call("POST", "/v1/admin/authority-limits", body=body)
        if not check(f"seed: {kind} limit requested", s in (201, 202), f"HTTP {s} {brief(lim)}"):
            return
    if lim.get("status") == "PENDING_APPROVAL":
        s, _, b = call("POST", f"/v1/admin/authority-limits/{lim['authority_limit_id']}/approve", principal=CHECKER,
                       body={"reason_code": "PERSONA_SEED"}, if_match=etag(lim["version"]))
        check(f"seed: {kind} limit approved by the checker", s == 200, f"HTTP {s} {brief(b)}")


def ensure_workload_binding():
    s, _, b = call("POST", "/v1/admin/workload-bindings", body={"workload_id": WORKLOAD, "allowed_audience": "zt-ap-api",
                   "allowed_actions": [VIEW], "reason_code": "PERSONA_SEED", "purpose": "nightly invoice batch"})
    check("seed: workload binding saved", s in (200, 201), f"HTTP {s} {brief(b)}")
    if s in (200, 201) and not b.get("active_flag", True):
        call("POST", f"/v1/admin/workload-bindings/{WORKLOAD}/reactivate", body={"reason_code": "PERSONA_SEED"},
             if_match=etag(b["version"]))


def seed():
    role_ids = {code: ensure_role(code, actions) for code, actions in ROLES.items()}
    assignments = {}
    for principal, code in GRANTS:
        if role_ids.get(code):
            # Administration powers are checked at tenant scope, so the checker's
            # role is granted tenant-wide (legal_entity_id ""), as the demo seed
            # grants the admin's; the business personas hold the one entity.
            entity = "" if code == "ZT_IAM_CHECKER" else ENTITY
            assignments[(principal, code)] = ensure_assignment(principal, role_ids[code], code, entity)
    ensure_sod_rule()
    ensure_abac_rule()
    ensure_limit("MONETARY", "10000.00", "GBP")
    ensure_limit("QUANTITY", "50")
    ensure_workload_binding()
    return role_ids, assignments


# ── scenarios ────────────────────────────────────────────────────────────────

def pipeline(role_ids):
    print("\n-- decision pipeline, layer by layer --")
    expect("RBAC: clerk may prepare", CLERK, PREPARE, "PERMIT", "rbac:")
    expect("RBAC: clerk may not release (no grant)", CLERK, RELEASE, "DENY", "no_grant", codes=["NO_MATCHING_GRANT"])
    expect("workload: bound action permitted", WORKLOAD, VIEW, "PERMIT", "rbac:", audience="zt-ap-api", principal_type="WORKLOAD")
    expect("workload: RBAC grant outside the binding denied", WORKLOAD, PREPARE, "DENY", "workload:action_not_bound",
           audience="zt-ap-api", principal_type="WORKLOAD")
    expect("workload: wrong audience denied (A18)", WORKLOAD, VIEW, "DENY", "workload:audience_mismatch",
           audience="some-other-api", principal_type="WORKLOAD")
    expect("workload: detected without declaring itself (N-09)", WORKLOAD, PREPARE, "DENY", "workload:")
    expect("dynamic SoD: requestor approving own request", APPROVER, APPROVE, "DENY", "sod:requestor_self_approval",
           attributes={"requestor_id": APPROVER, "amount": "100", "currency": "GBP"})
    expect("dynamic SoD: bank-detail changer releasing in cooling window (A06)", APPROVER, RELEASE, "DENY",
           "sod:cooling_window_conflict", attributes={"supplier_bank_changed_by": APPROVER, "cooling_window_active": "true",
                                                       "amount": "100", "currency": "GBP"})
    expect("dynamic SoD: prior rejecting reviewer re-approving", APPROVER, APPROVE, "DENY", "sod:prior_rejected_reviewer",
           attributes={"prior_rejected_by": APPROVER, "is_resubmission": "true", "amount": "100", "currency": "GBP"})
    expect("dynamic SoD: declared related party", APPROVER, APPROVE, "DENY", "sod:related_party_conflict",
           attributes={"related_party_subject_id": APPROVER, "amount": "100", "currency": "GBP"})
    expect("ABAC: restricted payment release forbidden", APPROVER, RELEASE, "DENY", "abac:",
           attributes={"classification": "restricted", "amount": "100", "currency": "GBP"})
    expect("limit: 9,000 GBP within 10,000 GBP", APPROVER, RELEASE, "PERMIT", "rbac:",
           attributes={"amount": "9000", "currency": "GBP", "quantity": "10"})
    expect("limit: 10,001 GBP over the limit", APPROVER, RELEASE, "DENY", "authority_limit:exceeded:upper_limit",
           attributes={"amount": "10001", "currency": "GBP", "quantity": "10"})
    expect("limit: 13,000 USD over after FX (A09)", APPROVER, RELEASE, "DENY", "authority_limit:exceeded:after_fx_conversion",
           attributes={"amount": "13000", "currency": "USD", "quantity": "10"}, codes=["FX_BASIS:REFERENCE_RATES:USD/GBP"])
    expect("limit: 12,000 USD within after FX, basis recorded", APPROVER, RELEASE, "PERMIT", "rbac:",
           attributes={"amount": "12000", "currency": "USD", "quantity": "10"}, codes=["FX_BASIS:REFERENCE_RATES:USD/GBP"])
    expect("limit: amount without currency refused (new)", APPROVER, RELEASE, "DENY", "authority_limit:currency_required",
           attributes={"amount": "100", "quantity": "10"}, codes=["CURRENCY_REQUIRED"])
    expect("limit: 51 units over the quantity limit", APPROVER, RELEASE, "DENY", "authority_limit:quantity_exceeded",
           attributes={"amount": "100", "currency": "GBP", "quantity": "51"})
    expect("quorum: one independent approval of two", APPROVER, RELEASE, "DENY", "quorum:insufficient_approvals",
           attributes={"amount": "100", "currency": "GBP", "quantity": "1", "required_approvals": "2",
                       "approved_by": json.dumps([CHECKER, CLERK]), "preparer_id": CLERK})
    expect("quorum: two independent approvals", APPROVER, RELEASE, "PERMIT", "rbac:",
           attributes={"amount": "100", "currency": "GBP", "quantity": "1", "required_approvals": "2",
                       "approved_by": json.dumps([CHECKER, ADMIN]), "preparer_id": CLERK})
    expect("revalidation: facts changed after approval (A10)", APPROVER, RELEASE, "DENY", "revalidation:fact_hash_mismatch",
           attributes={"amount": "100", "currency": "GBP", "quantity": "1", "approved_fact_hash": "h1", "current_fact_hash": "h2"})
    expect("revalidation: approved hash without current refused (new)", APPROVER, RELEASE, "DENY",
           "revalidation:current_fact_hash_missing",
           attributes={"amount": "100", "currency": "GBP", "quantity": "1", "approved_fact_hash": "h1"})
    expect("step-up: reopen with a 1-hour-old login (A26)", APPROVER, REOPEN, "STEP_UP", "assurance:recent_authn_required",
           attributes={"authn_age_seconds": "3600"})
    expect("step-up: reopen with a fresh login", APPROVER, REOPEN, "PERMIT", "rbac:", attributes={"authn_age_seconds": "60"})
    expect("state: approving a cancelled payment", APPROVER, APPROVE, "DENY", "state:terminal_status",
           attributes={"status": "CANCELLED", "amount": "100", "currency": "GBP"})
    s, b = authorize(CLERK, PREPARE)
    did = b.get("access_decision_id") or b.get("decision_id")
    check("evidence: decision artefact id and policy version returned", s == 200 and did and b.get("policy_set_version"),
          f"HTTP {s} {brief(b)}")
    return did


def static_sod(role_ids):
    print("\n-- static segregation of duties --")
    s, _, b = call("POST", "/v1/admin/role-assignments", body={
        "principal_id": CLERK, "role_id": role_ids["ZT_AP_APPROVER"], "legal_entity_id": ENTITY,
        "effective_from": datetime.now(timezone.utc).isoformat(), "approval_reference": "ZT"})
    check("SoD: granting release to a preparer refused (409)", s == 409 and "sod_conflict" in (b.get("error"), b.get("code")),
          f"HTTP {s} {brief(b)}")
    s, _, b = call("GET", f"/v1/sod/conflicting-permissions?action_type={PREPARE}", write=False)
    check("SoD: conflicting permissions lists release for prepare", s == 200 and RELEASE in json.dumps(b), f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", "/v1/sod/validate", body={"principal_id": CLERK, "candidate_actions": [RELEASE], "legal_entity_id": ENTITY,
                                                     "tenant_id": TENANT}, write=False)
    check("SoD: pre-flight validate reports the conflict", s == 200 and ("conflict" in json.dumps(b).lower()), f"HTTP {s} {brief(b)}")


def suspension(assignments):
    print("\n-- assignment suspension (ORG-06) and If-Match (AZ-1) --")
    aid = assignments.get((LEAVER, "ZT_INVOICE_VIEWER"))
    if not check("leaver holds an assignment", aid):
        return
    expect("leaver may view before suspension", LEAVER, VIEW, "PERMIT", "rbac:")
    s, _, b = call("POST", f"/v1/admin/role-assignments/{aid}/suspend", principal=CHECKER,
                   body={"reason_code": "INVESTIGATION", "purpose": "persona scenario"})
    check("If-Match: suspend without one is 428", s == 428, f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", f"/v1/admin/role-assignments/{aid}/suspend", principal=CHECKER,
                   body={"reason_code": "INVESTIGATION"}, if_match=etag(9999))
    check("If-Match: stale version is 412", s == 412, f"HTTP {s} {brief(b)}")
    s, _, rows = call("GET", f"/v1/admin/role-assignments?principal_id={LEAVER}")
    ver = next((a["version"] for a in (rows if isinstance(rows, list) else []) if a["principal_role_assignment_id"] == aid), 0)
    s, h, b = call("POST", f"/v1/admin/role-assignments/{aid}/suspend", principal=CHECKER,
                   body={"reason_code": "INVESTIGATION", "purpose": "persona scenario"}, if_match=etag(ver))
    check("suspend with the current version", s == 200 and b.get("approval_status") == "SUSPENDED", f"HTTP {s} {brief(b)}")
    expect("suspended leaver denied", LEAVER, VIEW, "DENY", "no_grant")
    s, _, b = call("POST", f"/v1/admin/role-assignments/{aid}/resume", principal=LEAVER,
                   body={"reason_code": "RETURNED"}, if_match=etag(b.get("version", ver + 1)))
    check("resume of one's own assignment refused", s == 403, f"HTTP {s} {brief(b)}")
    s, _, rows = call("GET", f"/v1/admin/role-assignments?principal_id={LEAVER}&include_expired=true")
    ver = next((a["version"] for a in (rows if isinstance(rows, list) else []) if a["principal_role_assignment_id"] == aid), 0)
    s, _, b = call("POST", f"/v1/admin/role-assignments/{aid}/resume", principal=CHECKER,
                   body={"reason_code": "RETURNED", "purpose": "investigation closed"}, if_match=etag(ver))
    check("resume by the checker", s == 200 and b.get("approval_status") == "APPROVED", f"HTTP {s} {brief(b)}")
    expect("resumed leaver permitted again", LEAVER, VIEW, "PERMIT", "rbac:")
    s, _, b = call("GET", f"/v1/admin/role-assignments/{aid}/suspensions", principal=CHECKER)
    check("suspension history recorded", s == 200 and len(b if isinstance(b, list) else b.get("suspensions", [])) >= 1, f"HTTP {s} {brief(b)}")
    past = (datetime.now(timezone.utc) - timedelta(seconds=2)).strftime("%Y-%m-%dT%H:%M:%SZ")
    s, _, b = call("GET", f"/v1/admin/role-assignments?as_of={past}&principal_id={LEAVER}", principal=CHECKER)
    check("as-of read answers", s == 200 and isinstance(b, list), f"HTTP {s} {brief(b)}")
    future = (datetime.now(timezone.utc) + timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    s, _, b = call("GET", f"/v1/admin/role-assignments?as_of={future}", principal=CHECKER)
    check("as-of in the future refused", s == 400 and "invalid_as_of" in (b.get("error"), b.get("code")), f"HTTP {s} {brief(b)}")


def idempotency(role_ids):
    print("\n-- idempotency --")
    key = str(uuid.uuid4())
    body = {"tenant_id": TENANT, "role_code": "ZT_IDEMPOTENT_" + key[:6].upper(), "role_name": "Idempotency probe",
            "role_scope_type": "TENANT", "created_by_principal_id": ADMIN}
    s1, _, b1 = call("POST", "/v1/admin/roles", body=body, key=key)
    s2, h2, b2 = call("POST", "/v1/admin/roles", body=body, key=key)
    check("retry with the same key replays the first answer", s1 == 201 and s2 in (200, 201) and b1.get("role_id") == b2.get("role_id")
          and (h2.get("X-Idempotent-Replay") == "true"), f"{s1}/{s2} replay={h2.get('X-Idempotent-Replay')}")
    s3, _, b3 = call("POST", "/v1/admin/roles", body={**body, "role_name": "different"}, key=key)
    check("same key, different body is 409", s3 == 409, f"HTTP {s3} {brief(b3)}")


def paging():
    print("\n-- cursor paging (API-4) --")
    s, _, b = call("GET", "/v1/admin/roles?page_size=2")
    ok = s == 200 and isinstance(b.get("records"), list) and len(b["records"]) <= 2
    check("roles page of 2 with a cursor", ok and b.get("next_cursor"), f"HTTP {s} {brief(b)}")
    seen, cur, pages = [], b.get("next_cursor"), 1
    seen += [r["role_id"] for r in b.get("records", [])]
    while cur and pages < 200:
        s, _, b = call("GET", "/v1/admin/roles?page_size=2&cursor=" + urllib.parse.quote(cur))
        seen += [r["role_id"] for r in b.get("records", [])]
        cur, pages = b.get("next_cursor"), pages + 1
    s, _, flat = call("GET", "/v1/admin/roles")
    check("cursor traversal equals the unpaged register", sorted(seen) == sorted(r["role_id"] for r in flat) and len(seen) == len(set(seen)),
          f"paged {len(seen)} vs flat {len(flat) if isinstance(flat, list) else flat}")
    s, _, b = call("GET", f"/v1/admin/role-assignments?page_size=3&principal_id={CHECKER}")
    check("assignments page (SQL keyset)", s == 200 and isinstance(b.get("records"), list), f"HTTP {s} {brief(b)}")
    s, _, b = call("GET", "/v1/admin/roles?cursor=forged")
    check("forged cursor refused", s == 400, f"HTTP {s} {brief(b)}")


def iam_aliases():
    print("\n-- §21 /v1/iam/delegations --")
    end = (datetime.now(timezone.utc) + timedelta(days=7)).isoformat()
    # The admin holds iam.delegation.grant; the delegator must be the caller.
    s, _, d = call("POST", "/v1/iam/delegations", principal=ADMIN, body={
        "delegator_principal_id": ADMIN, "delegate_principal_id": CLERK, "scope_type": "ACTIONS",
        "delegated_actions": [VIEW], "legal_entity_id": ENTITY, "effective_from": datetime.now(timezone.utc).isoformat(),
        "effective_to": end, "reason": "holiday cover", "reason_code": "HOLIDAY_COVER"})
    did = d.get("delegated_authority_id")
    if not check("delegation granted through the §21 alias", s == 201 and did, f"HTTP {s} {brief(d)}"):
        return
    s, _, b = call("GET", "/v1/iam/delegations", principal=ADMIN)
    check("delegation listed through the alias", s == 200 and did in json.dumps(b), f"HTTP {s}")
    s, _, b = call("POST", f"/v1/iam/delegations/{did}:revoke", principal=CLERK, body={"reason_code": "X"})
    check("only the delegator may revoke (403)", s == 403, f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", f"/v1/iam/delegations/{did}:revoke", principal=ADMIN, body={"reason_code": "COVER_ENDED"})
    check("delegator revokes through :revoke", s == 200, f"HTTP {s} {brief(b)}")


def decision_log(did):
    print("\n-- decision log (GOV-03 ExplainAuthorization) --")
    s, _, b = call("GET", f"/v1/access-decisions?principal_id={CLERK}&limit=5", principal=CHECKER)
    items = b.get("items") or b.get("decisions") or b.get("records") or []
    check("checker (iam.policy.read) reads the clerk's decisions", s == 200 and len(items) >= 1, f"HTTP {s} {brief(b)}")
    s, _, b = call("GET", f"/v1/access-decisions?principal_id={CLERK}", principal=CLERK)
    items = b.get("items") or b.get("decisions") or b.get("records") or []
    check("clerk reads own decisions, basis redacted", s == 200 and items and all(i.get("decision_basis") in ("redacted", None) for i in items),
          f"HTTP {s} {brief(items[:1])}")
    s, _, b = call("GET", f"/v1/access-decisions?principal_id={APPROVER}", principal=CLERK)
    check("clerk asking for another's decisions is 403", s == 403, f"HTTP {s}")
    if did:
        s, _, b = call("GET", f"/v1/access-decisions/{did}", principal=CHECKER)
        check("decision by id", s == 200 and (b.get("access_decision_id") == did), f"HTTP {s} {brief(b)}")


def surfaces():
    print("\n-- capabilities, available actions, validators, GOV-03 --")
    s, _, b = call("GET", f"/v1/payment/pmt-zt-1/available-actions?legal_entity_id={ENTITY}", principal=APPROVER, write=False)
    check("available actions for the approver", s == 200 and RELEASE in json.dumps(b), f"HTTP {s} {brief(b)}")
    s, _, b = call("GET", f"/v1/me/capabilities?legal_entity_id={ENTITY}", principal=CLERK, write=False)
    check("clerk's own capabilities", s == 200 and PREPARE in json.dumps(b), f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", "/v1/entity-scope/validate", body={"principal_id": CLERK, "legal_entity_ids": [ENTITY], "tenant_id": TENANT}, write=False)
    check("entity-scope validate answers", s == 200, f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", "/v1/sod/evaluate", body={"action_type": APPROVE, "maker_principal_id": APPROVER,
                                                     "checker_principal_id": APPROVER}, write=False)
    check("maker-checker evaluate refuses maker = checker", s == 200 and json.dumps(b).lower().count("conflict") >= 1, f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", "/v1/admin/authorization-cache/invalidate", body={"reason_code": "PERSONA_SCENARIO",
                   "purpose": "prove the GOV-03 command"})
    check("GOV-03 cache invalidation", s == 200, f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", f"/v1/admin/subjects/{CLERK}/effective-access/recompute", principal=CHECKER, body={"reason_code": "PERSONA"})
    check("GOV-03 effective access recompute", s == 200 and PREPARE in json.dumps(b), f"HTTP {s} {brief(b)}")


def sessions():
    print("\n-- privileged and support sessions --")
    s, _, ss = call("POST", "/v1/support/sessions", body={"support_operator_id": SUPPORT, "ticket_ref": "ZT-SUP-1",
                    "purpose": "invoice display bug", "duration_seconds": 1800, "tenant_consent_obtained": True,
                    "allowed_actions": []})
    sid = ss.get("session_id")
    if check("support session issued (read-only, consent stated)", s in (200, 201) and sid, f"HTTP {s} {brief(ss)}"):
        expect("support: read permitted", SUPPORT, VIEW, "PERMIT", "support:", support_session_id=sid)
        expect("support: write refused in a read-only session", SUPPORT, PREPARE, "DENY", "support:read_only_session",
               support_session_id=sid)
        expect("support: bulk export refused (A14)", SUPPORT, EXPORT, "DENY", "support:bulk_export_prohibited",
               support_session_id=sid)
        s, _, cur = call("GET", f"/v1/support/sessions/{sid}")
        s, _, b = call("POST", f"/v1/support/sessions/{sid}/revoke", body={"reason_code": "DONE"},
                       if_match=etag(cur.get("record_version", 1)))
        check("support session revoked", s == 200, f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", "/v1/admin/privileged-sessions", body={"principal_id": ADMIN, "requested_actions": [VIEW],
                   "ticket_ref": "ZT-JIT-1", "reason": "persona", "duration_seconds": 600, "resource_scope": ENTITY,
                   "target_environment": "local"})
    check("JIT without a verified step-up refused", s in (401, 403), f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", "/v1/admin/privileged-sessions", body={"principal_id": ADMIN, "requested_actions": [VIEW],
                   "ticket_ref": "ZT-JIT-2", "reason": "persona", "duration_seconds": 9 * 3600, "resource_scope": ENTITY,
                   "target_environment": "local"})
    check("JIT longer than 8 h refused", s in (400, 401, 403), f"HTTP {s} {brief(b)}")


def telemetry():
    print("\n-- §26 telemetry --")
    req = urllib.request.Request(AUTHZ + "/metrics")
    try:
        text = urllib.request.urlopen(req, timeout=10).read().decode()
    except Exception as e:  # noqa: BLE001
        check("metrics endpoint", False, str(e))
        return
    for fam in ["authorization_decisions_total", "authorization_decisions_by_action_total", "authorization_evaluation_seconds_bucket",
                "authorization_cache_hits_total", "authorization_watermark_age_seconds", "authorization_outbox_pending",
                "authorization_sessions{", "authorization_projection_lag_seconds_count"]:
        check(f"metric {fam.rstrip('{')}", fam in text)
    check("decisions counted by deciding layer (sod)", 'layer="sod"' in text)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seed-only", action="store_true")
    a = ap.parse_args()
    s, _, b = call("GET", "/readyz", write=False)
    if not check("authorization-svc ready", s == 200, f"HTTP {s} {brief(b)}"):
        return 1
    print("-- seeding personas --")
    role_ids, assignments = seed()
    if not a.seed_only:
        did = pipeline(role_ids)
        static_sod(role_ids)
        suspension(assignments)
        idempotency(role_ids)
        paging()
        iam_aliases()
        decision_log(did)
        surfaces()
        sessions()
        time.sleep(16)  # one sampler period for the session / outbox gauges
        telemetry()
    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} passed")
    return len(failed)


if __name__ == "__main__":
    sys.exit(main())
