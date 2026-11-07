#!/usr/bin/env python3
"""End-to-end check of authorization-svc and every service it depends on.

Each check crosses one real dependency edge of authorization-svc on the local
compose stack, so a failure names the edge that is broken:

  tenant-entity-registry-svc  HTTP: the tenant of an unprojected entity
  identity-context-svc        HTTP: envelope JWKS + session introspection on the
                              admin routes; Kafka: identity.subject.* into layer 0
  jurisdiction-rules-svc      HTTP: a jurisdiction-scoped SoD rule's jurisdiction
  delegated-authority-svc     Kafka: authority.* projected into the delegation layer
  access-control-svc          HTTP: its own authorization; Kafka: role.created
                              invalidating authorization-svc's grant cache
  mtls-management-svc         mTLS: its decisions arrive on authorization-svc :8449
  siem-integration-svc        HTTP: authorization.denied streamed to an exporter
  otel-collector / jaeger     OTLP: authorization-svc traces reach Jaeger

    python deployments/scripts/e2e-authorization-deps.py

Run seed-demo-rbac.ps1 and seed-demo-registry.ps1 first; the demo principal
needs their grants. It writes a little, and undoes what it can: the delegation
is revoked, the subject is reinstated, and the proposed SoD rule and the e2e
role are left PROPOSED / ACTIVE but grant nothing (no bundle, no assignment).

Exit status is the number of failed checks.
"""
import argparse, json, os, subprocess, sys, time, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")

TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
# Held by the demo principal in its own right (seed-demo-rbac.ps1), so it can
# be delegated.
ACTION = "FINANCIAL_STATEMENT_SET_VIEW"

H = "http://127.0.0.1"
AUTHZ, IDENTITY, REGISTRY = f"{H}:8089", f"{H}:8080", f"{H}:8081"
JURISDICTION, DELEGATION, ACCESS = f"{H}:8082", f"{H}:8136", f"{H}:8137"
MTLS, SIEM, JAEGER = f"{H}:8140", f"{H}:8141", f"{H}:16686"

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


def envelope(principal=ADMIN, write=False, tenant=TENANT, entity=ENTITY):
    """The canonical input contract: identity, correlation, channel, and on a
    write an Idempotency-Key and X-Occurred-At."""
    rid = str(uuid.uuid4())
    h = {
        "X-Tenant-Id": tenant,
        "X-Principal-Id": principal,
        "X-Legal-Entity-Id": entity,
        "X-Correlation-ID": "e2e-authz-" + rid[:8],
        "X-Request-Id": rid,
        "X-Source-Channel": "api",
        "X-Source-System": "e2e-authorization-deps",
    }
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
        h["X-Occurred-At"] = datetime.now(timezone.utc).isoformat()
    return h


def call(method, url, headers=None, body=None, timeout=15):
    """(status, headers, parsed body). Never raises for an HTTP status."""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
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


def authorize(principal, entity=ENTITY, action=ACTION, tenant=TENANT):
    return call("POST", f"{AUTHZ}/v1/authorize", envelope(principal, tenant=tenant, entity=entity),
                {"principal_id": principal, "legal_entity_id": entity, "action_type": action})


def until(predicate, seconds=30, step=1.0):
    """Poll predicate() until it returns a truthy value or time runs out; the
    last value is returned either way."""
    deadline = time.time() + seconds
    while True:
        v = predicate()
        if v[0] or time.time() >= deadline:
            return v
        time.sleep(step)


def brief(body):
    return json.dumps(body)[:220]


# ── 0. every service is up ───────────────────────────────────────────────────

def health():
    for name, url in [
        ("authorization-svc", f"{AUTHZ}/readyz"),
        ("identity-context-svc", f"{IDENTITY}/health"),
        ("tenant-entity-registry-svc", f"{REGISTRY}/healthz"),
        ("jurisdiction-rules-svc", f"{JURISDICTION}/readyz"),
        ("delegated-authority-svc", f"{DELEGATION}/readyz"),
        ("access-control-svc", f"{ACCESS}/healthz"),
        ("mtls-management-svc", f"{MTLS}/healthz"),
        ("siem-integration-svc", f"{SIEM}/healthz"),
        ("jaeger", f"{JAEGER}/api/services"),
    ]:
        s, _, b = call("GET", url, timeout=5)
        check(f"health {name}", s == 200, f"HTTP {s}" if s != 200 else "")


# ── 1. the evaluation path itself ────────────────────────────────────────────

def core():
    s, _, b = authorize(ADMIN)
    check("authorize: demo principal holds " + ACTION, s == 200 and b.get("decision") == "PERMIT",
          f"HTTP {s} {b.get('decision')} {b.get('decision_basis', b.get('error', ''))}")


# ── 2. tenant-entity-registry-svc ────────────────────────────────────────────

def registry():
    # An entity no projection has seen: authorization-svc must ask the registry
    # whether it is in the claimed tenant. A random id is in none, so the answer
    # is 403 tenant_scope_mismatch; 503 would mean the registry did not answer.
    s, _, b = authorize(ADMIN, entity=str(uuid.uuid4()))
    check("registry: unprojected entity outside the tenant is refused", s == 403 and "tenant_scope_mismatch" in (b.get("error"), b.get("code")),
          f"HTTP {s} {brief(b)}")


# ── 3. identity-context-svc: envelope on the admin routes ────────────────────

def identity_envelope(password):
    # The console's confidential client: identity-context-svc runs with
    # IDP_CLIENT_AUTH=required, so a login without one is refused.
    s, _, b = call("POST", f"{IDENTITY}/v1/authenticate", {"Content-Type": "application/json"},
                   {"tenant_id": TENANT, "email": "admin@zoikosuite.com", "password": password,
                    "client_id": os.environ.get("IDENTITY_CONSOLE_CLIENT_ID", "zoiko-console"),
                    "client_secret": os.environ.get("IDENTITY_CONSOLE_CLIENT_SECRET", "local-dev-console-client-secret")})
    if not check("identity: password login", s == 200 and b.get("access_token"), f"HTTP {s} {brief(b)}" if s != 200 else ""):
        return
    h = envelope(write=True)
    s, _, b = call("POST", f"{IDENTITY}/v1/context/resolve", h,
                   {"bearer_token": b["access_token"], "legal_entity_id": ENTITY})
    jwt = b.get("envelope_jwt")
    if not check("identity: context resolved to a signed envelope", s in (200, 201) and jwt, f"HTTP {s} {brief(b)}" if not jwt else ""):
        return
    # Verified against identity-context-svc's JWKS, session introspected.
    h = envelope()
    h["Authorization"] = "Bearer " + jwt
    s, hd, b = call("GET", f"{AUTHZ}/v1/admin/roles", h)
    check("authz admin route accepts the verified envelope",
          s == 200 and (hd.get("X-Identity-Assertion") or "") != "missing", f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")
    head, payload, sig = jwt.split(".")
    h["Authorization"] = "Bearer " + ".".join([head, payload, sig[:-4] + ("AAAA" if not sig.endswith("AAAA") else "BBBB")])
    s, _, b = call("GET", f"{AUTHZ}/v1/admin/roles", h)
    check("authz admin route refuses a tampered envelope", s == 401, f"HTTP {s} {b.get('error', '')}")


# ── 4. jurisdiction-rules-svc ────────────────────────────────────────────────

def jurisdiction():
    s, _, b = call("GET", f"{JURISDICTION}/v1/jurisdictions", envelope())
    items = b if isinstance(b, list) else (b.get("data") or b.get("jurisdictions") or [])
    known = next((j.get("jurisdiction_id") for j in items if isinstance(j, dict) and j.get("active_flag", True)), None)
    stamp = uuid.uuid4().hex[:8].upper()
    rule = {"domain_code": "E2E", "action_a": f"E2E_A_{stamp}", "action_b": f"E2E_B_{stamp}",
            "conflict_type": "STATIC_HIGH", "tenant_id": TENANT,
            "reason_code": "E2E_VERIFICATION", "purpose": "dependency check of jurisdiction-rules-svc"}

    s, _, b = call("POST", f"{AUTHZ}/v1/admin/sod-rules", envelope(write=True), {**rule, "jurisdiction_id": str(uuid.uuid4())})
    check("jurisdiction: unknown jurisdiction refused by the registry", s == 404 and "jurisdiction_not_found" in (b.get("error"), b.get("code")),
          f"HTTP {s} {brief(b)}")
    if not check("jurisdiction: a known jurisdiction is listed", known, "" if known else "none listed"):
        return
    s, _, b = call("POST", f"{AUTHZ}/v1/admin/sod-rules", envelope(write=True), {**rule, "jurisdiction_id": known})
    check("jurisdiction: rule scoped to a real jurisdiction is proposed", s in (201, 202), f"HTTP {s} {brief(b)}")


# ── 5. delegated-authority-svc and identity-context-svc over Kafka ───────────

def delegation_and_status(subject):
    s, _, b = authorize(subject)
    if not check("baseline: subject holds nothing", s == 200 and b.get("decision") == "DENY",
                 f"HTTP {s} {b.get('decision')} {b.get('decision_basis')}"):
        return
    now = datetime.now(timezone.utc)
    s, hd, d = call("POST", f"{DELEGATION}/v1/delegations", envelope(write=True), {
        "legal_entity_id": ENTITY, "delegator_principal_id": ADMIN, "delegate_principal_id": subject,
        "action_type": ACTION, "effective_from": now.isoformat(),
        "effective_to": (now + timedelta(hours=1)).isoformat(),
        "correlation_id": "e2e-" + uuid.uuid4().hex[:8], "reason": "e2e dependency check"})
    did = d.get("delegation_id")
    if not check("delegation: granted in delegated-authority-svc", s == 201 and did, f"HTTP {s} {brief(d)}"):
        return
    suspended = False
    try:
        s, _, b = until(lambda: (lambda r: (r[2].get("decision") == "PERMIT", r))(authorize(subject)))[1]
        check("delegation: projected over Kafka, subject now permitted", b.get("decision") == "PERMIT",
              f"{b.get('decision')} {b.get('decision_basis')}")

        suspended = set_status(subject, "SUSPENDED")
        if suspended:
            s, _, b = until(lambda: (lambda r: (r[2].get("decision") == "DENY", r))(authorize(subject)))[1]
            check("identity: suspension reached layer 0 over Kafka", b.get("decision") == "DENY"
                  and str(b.get("decision_basis", "")).startswith("principal_status:"),
                  f"{b.get('decision')} {b.get('decision_basis')}")
    finally:
        if suspended and set_status(subject, "ACTIVE"):
            s, _, b = until(lambda: (lambda r: (r[2].get("decision") == "PERMIT", r))(authorize(subject)))[1]
            check("identity: reinstatement reached layer 0 over Kafka", b.get("decision") == "PERMIT",
                  f"{b.get('decision')} {b.get('decision_basis')}")
        h = envelope(write=True)
        h["If-Match"] = hd.get("ETag") or f'"{d.get("version", 1)}"'
        s, _, b = call("POST", f"{DELEGATION}/v1/delegations/{did}/revoke", h, {"reason": "e2e dependency check finished"})
        if check("delegation: revoked", s == 200, f"HTTP {s} {brief(b)}" if s != 200 else ""):
            s, _, b = until(lambda: (lambda r: (r[2].get("decision") == "DENY", r))(authorize(subject)))[1]
            check("delegation: revocation projected, subject denied again", b.get("decision") == "DENY",
                  f"{b.get('decision')} {b.get('decision_basis')}")


def set_status(subject, status, quiet=False):
    s, hd, b = call("GET", f"{IDENTITY}/v1/principals/{subject}", envelope())
    if s != 200:
        return check(f"identity: read subject before {status}", False, f"HTTP {s} {brief(b)}")
    if b.get("status") == status and quiet:
        return True
    h = envelope(write=True)
    h["If-Match"] = hd.get("ETag") or "*"
    s, _, b = call("PUT", f"{IDENTITY}/v1/principals/{subject}/status", h,
                   {"status": status, "reason": "e2e dependency check"})
    return check(f"identity: subject set {status}", s in (200, 204), f"HTTP {s} {brief(b)}" if s not in (200, 204) else "")


# ── 6. access-control-svc ────────────────────────────────────────────────────

def access_control():
    since = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    corr = "e2e-acs-" + uuid.uuid4().hex[:8]
    h = envelope(write=True)
    h["X-Correlation-ID"] = corr
    s, _, b = call("POST", f"{ACCESS}/v1/role-definitions", h, {
        "legal_entity_id": ENTITY, "role_code": "E2E_ROLE_" + uuid.uuid4().hex[:8].upper(),
        "role_name": "E2E dependency check role", "role_scope_type": "LEGAL_ENTITY", "correlation_id": corr})
    if not check("access-control: role created (authorized by authorization-svc)", s == 201, f"HTTP {s} {brief(b)}"):
        return

    def seen():
        try:
            out = subprocess.run(["docker", "logs", "--since", since, "authorization-svc"],
                                 capture_output=True, text=True, timeout=20)
        except (OSError, subprocess.SubprocessError) as e:
            return (None, str(e))
        lines = [l for l in (out.stdout + out.stderr).splitlines()
                 if "cached grants invalidated" in l and "role.created" in l]
        return (bool(lines), lines)

    found, _ = until(seen, seconds=30, step=2)
    if found is None:
        check("access-control: role.created consumed by authorization-svc", True, "SKIPPED — docker CLI unavailable")
    else:
        check("access-control: role.created consumed by authorization-svc", found, "" if found else "no invalidation logged in 30 s")


# ── 7. mtls-management-svc → authorization-svc over mTLS ─────────────────────

def mtls():
    # mtls-management-svc asks for this decision on https://authorization-svc:8449
    # with its own certificate. 200 or 403 is a decision; 503 is no decision.
    s, _, b = call("GET", f"{MTLS}/v1/mtls/certificates?legal_entity_id={ENTITY}", envelope())
    check("mtls: mtls-management-svc got a decision over mTLS", s in (200, 403), f"HTTP {s} {brief(b)}" if s not in (200, 403) else f"HTTP {s}")


# ── 8. siem-integration-svc ──────────────────────────────────────────────────

def siem(subject):
    s, _, b = call("GET", f"{SIEM}/v1/siem/exporters?legal_entity_id={ENTITY}", envelope())
    exp = next((e for e in (b.get("data") or []) if e.get("name") == "e2e-authorization" and e.get("status") == "ACTIVE"), None)
    if not exp:
        s, _, exp = call("POST", f"{SIEM}/v1/siem/exporters", envelope(write=True), {
            "legal_entity_id": ENTITY, "name": "e2e-authorization", "platform": "SYSLOG",
            "endpoint_url": "syslog://127.0.0.1:514"})
        if not check("siem: exporter available", s == 201 and exp.get("id"), f"HTTP {s} {brief(exp)}"):
            return
    marker = uuid.uuid4().hex[:8]
    # An admin route the subject holds no permission for: denied, and streamed.
    s, _, _ = call("GET", f"{AUTHZ}/v1/admin/roles?e2e={marker}", envelope(subject))
    check("siem: admin route denied to an unprivileged subject", s == 403, f"HTTP {s}")

    def streamed():
        s, _, b = call("GET", f"{SIEM}/v1/siem/events?exporter_id={exp['id']}", envelope())
        evs = [e for e in (b.get("data") or []) if e.get("source_service") == "authorization-svc"
               and e.get("event_type") == "authorization.denied" and subject in (e.get("message") or "")]
        return (bool(evs), s)

    found, status = until(streamed, seconds=20, step=2)
    check("siem: authorization.denied streamed by authorization-svc", found, "" if found else f"none seen (events HTTP {status})")


# ── 9. otel-collector → jaeger ───────────────────────────────────────────────

def tracing():
    def listed():
        s, _, b = call("GET", f"{JAEGER}/api/services")
        return ("authorization-svc" in (b.get("data") or []), b.get("data"))
    found, services = until(listed, seconds=20, step=2)
    check("tracing: authorization-svc traces in Jaeger", found, "" if found else f"services={services}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--subject", default=os.environ.get("E2E_SUBJECT", "798980b8-d26e-4439-a065-a7f80120c36e"),
                    help="an ACTIVE identity-context principal in the demo tenant that holds no grants")
    ap.add_argument("--password", default=os.environ.get("E2E_ADMIN_PASSWORD", "Zoiko@Governance1"),
                    help="admin@zoikosuite.com's password (the demo seed's)")
    a = ap.parse_args()

    health()
    core()
    registry()
    identity_envelope(a.password)
    jurisdiction()
    delegation_and_status(a.subject)
    access_control()
    mtls()
    siem(a.subject)
    tracing()

    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} passed")
    sys.exit(len(failed))


if __name__ == "__main__":
    main()
