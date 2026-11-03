#!/usr/bin/env python3
"""End-to-end check of identity-context-svc and the services it depends on.

Each check crosses one real dependency edge of identity-context-svc on the
local compose stack, so a failure names the edge that is broken:

  postgres / redis            login, resolve and the console read models
  tenant-entity-registry-svc  HTTP: the tenant and entity a resolve is scoped to
  authorization-svc           HTTP: every console read and command is authorized
                              there; Kafka: iam.assignment.* into the role
                              projection (IAM-09); envelope verification and
                              session introspection on its admin routes
  access-control-svc          the identity actions are registered (000026)
  mtls-management-svc         authorization-svc's server identity
  kafka outbox                identity.subject.merged and principal.status.changed

    python deployments/scripts/e2e-identity-context-deps.py

Run seed-demo-rbac.ps1 and seed-demo-registry.ps1 first; the demo admin needs
their grants. Writes are undone where they can be: the TOTP factor enrolled for
the step-up is revoked, the person created for the reset check is disabled.
Merged service accounts stay merged (a merge is final by design).

Exit status is the number of failed checks.
"""
import argparse, base64, hashlib, hmac, json, os, struct, subprocess, sys, time, urllib.error, urllib.request, uuid

sys.stdout.reconfigure(encoding="utf-8")

TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"

H = "http://127.0.0.1"
IDENTITY, AUTHZ, REGISTRY, ACCESS, MTLS = f"{H}:8080", f"{H}:8089", f"{H}:8081", f"{H}:8137", f"{H}:8140"

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


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


def hdrs(jwt=None, principal=ADMIN, write=False, if_match=None, tenant=TENANT):
    rid = str(uuid.uuid4())
    h = {"X-Tenant-Id": tenant, "X-Principal-Id": principal, "X-Legal-Entity-Id": ENTITY,
         "X-Correlation-ID": "e2e-ic-" + rid[:8], "X-Request-Id": rid, "X-Source-Channel": "api"}
    if jwt:
        h["Authorization"] = "Bearer " + jwt
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
    if if_match:
        h["If-Match"] = if_match
    return h


def brief(body):
    return json.dumps(body)[:220]


def psql(sql, db="identity_context"):
    out = subprocess.run(["docker", "exec", "zoiko-postgres", "psql", "-U", "postgres", "-d", db, "-Atc", sql],
                         capture_output=True, text=True)
    return out.stdout.strip()


def totp(secret_b32, at=None, step=30, digits=6):
    key = base64.b32decode(secret_b32 + "=" * (-len(secret_b32) % 8))
    counter = int((at or time.time()) // step)
    mac = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    off = mac[-1] & 0x0F
    code = (struct.unpack(">I", mac[off:off + 4])[0] & 0x7FFFFFFF) % (10 ** digits)
    return str(code).zfill(digits)


def login(password):
    s, _, b = call("POST", f"{IDENTITY}/v1/authenticate", {"Content-Type": "application/json"},
                   {"tenant_id": TENANT, "email": "admin@zoikosuite.com", "password": password})
    return s, b


def resolve(access_token):
    s, _, b = call("POST", f"{IDENTITY}/v1/context/resolve", hdrs(write=True),
                   {"bearer_token": access_token, "legal_entity_id": ENTITY})
    return s, b


# ── 0. every dependency is up ────────────────────────────────────────────────

def health():
    for name, url in [("identity-context-svc", f"{IDENTITY}/health"), ("authorization-svc", f"{AUTHZ}/readyz"),
                      ("tenant-entity-registry-svc", f"{REGISTRY}/healthz"), ("access-control-svc", f"{ACCESS}/healthz"),
                      ("mtls-management-svc", f"{MTLS}/healthz")]:
        s, _, b = call("GET", url, timeout=5)
        check(f"health {name}", s == 200, f"HTTP {s}" if s != 200 else "")
    s, _, b = call("GET", f"{IDENTITY}/health", timeout=5)
    checks = b.get("checks", {})
    check("identity health: postgres, redis, tenant registry, outbox",
          all(checks.get(k) == "ok" for k in ("postgres", "redis", "tenant_registry", "outbox")), brief(checks))


# ── 1. login and resolve: postgres, redis, registry, authorization ───────────

def session(password):
    s, b = login(password)
    if not check("login: password authentication", s == 200 and b.get("access_token"), f"HTTP {s} {brief(b)}" if s != 200 else ""):
        return None
    s, b = resolve(b["access_token"])
    jwt, sid = b.get("envelope_jwt"), b.get("session_context_id")
    check("resolve: registry + authorization answer, signed envelope issued", s in (200, 201) and jwt and sid,
          f"HTTP {s} {brief(b)}" if not jwt else f"session {sid}")
    s, _, jw = call("GET", f"{IDENTITY}/.well-known/jwks.json")
    check("JWKS served for relying parties", s == 200 and len(jw.get("keys", [])) > 0, f"HTTP {s}")
    return {"jwt": jwt, "sid": sid}


# ── 2. console read models (IC-2) ─────────────────────────────────────────────

def read_models(sess):
    h = hdrs(sess["jwt"])
    s, _, b = call("GET", f"{IDENTITY}/v1/identity/summary", h)
    check("summary: KPI counts read in one transaction", s == 200 and "principals_by_status" in b,
          f"HTTP {s} active={b.get('principals_by_status', {}).get('ACTIVE')} sessions={b.get('active_sessions')}")
    for path in ["/v1/principals?page_size=2", "/v1/context/sessions?filter[status]=ACTIVE", "/v1/context/support",
                 f"/v1/principals/{ADMIN}/assurance-history"]:
        s, _, b = call("GET", IDENTITY + path, h)
        check(f"list {path.split('?')[0]}", s == 200 and isinstance(b.get("items"), list), f"HTTP {s} {brief(b)}" if s != 200 else f"{len(b['items'])} rows")
    s, _, b = call("GET", f"{IDENTITY}/v1/principals?page_size=1", h)
    nxt = b.get("next_cursor", "")
    if check("principals: page 1 carries a signed next_cursor", s == 200 and nxt, f"HTTP {s}"):
        s2, _, b2 = call("GET", f"{IDENTITY}/v1/principals?page_size=1&cursor={nxt}", h)
        check("principals: next page follows", s2 == 200 and b2.get("items") and b2["items"][0]["principal_id"] != b["items"][0]["principal_id"], f"HTTP {s2}")
        p, mac = nxt.split(".")
        bad = ("A" if p[0] != "A" else "B") + p[1:] + "." + mac
        s3, _, b3 = call("GET", f"{IDENTITY}/v1/principals?page_size=1&cursor={bad}", h)
        check("principals: tampered cursor refused", s3 == 400 and b3.get("code") == "invalid_cursor", f"HTTP {s3} {b3.get('code')}")
        s4, _, b4 = call("GET", f"{IDENTITY}/v1/principals?page_size=1&filter[status]=DISABLED&cursor={nxt}", h)
        check("principals: cursor replayed against another filter refused", s4 == 400, f"HTTP {s4} {b4.get('code')}")
    s, _, b = call("GET", f"{IDENTITY}/v1/principals?filter[tenant_id]=x", h)
    check("principals: unknown filter refused", s == 400, f"HTTP {s} {b.get('code')}")
    # The login above was written to authentication_events in the same
    # transaction as its access_decision_log row.
    s, _, b = call("GET", f"{IDENTITY}/v1/authentication-events?filter[principal_id]={ADMIN}&page_size=5", h)
    logins = [e for e in b.get("items", []) if e.get("event_type") == "LOGIN" and e.get("outcome") == "SUCCESS"]
    check("sign-in events: this run's login recorded", s == 200 and logins, f"HTTP {s} {len(b.get('items', []))} events")


# ── 3. authorization-svc decides every console read ──────────────────────────

def authz_edge(sess):
    stranger = str(uuid.uuid4())
    s, _, b = call("GET", f"{IDENTITY}/v1/principals", hdrs(principal=stranger))
    check("authorization-svc refuses a principal with no grant", s == 403, f"HTTP {s} {b.get('code')}")
    rows = psql(f"SELECT COUNT(*) FROM principal_role_assignments WHERE principal_id = '{ADMIN}' AND tenant_id = '{TENANT}'")
    check("kafka iam.assignment.* → role projection (IAM-09)", rows.isdigit() and int(rows) > 0, f"{rows} projected assignments")
    acts = psql("SELECT string_agg(action_name, ',' ORDER BY action_name) FROM permission_definitions "
                "WHERE action_name IN ('IDENTITY_PRINCIPAL_CREATE','IDENTITY_AUTH_EVENTS_READ','IDENTITY_SUBJECT_MERGE',"
                "'IDENTITY_PASSWORD_RESET_REQUEST','IDENTITY_PASSWORD_RESET_APPROVE')", db="access_control")
    check("access-control: identity actions registered (000026)", acts.count(",") == 4, acts)


# ── 4. subject lifecycle (IC-1) ───────────────────────────────────────────────

def lifecycle(sess):
    created = {}
    for name in ("survivor", "duplicate"):
        h = hdrs(sess["jwt"], write=True)
        body = {"display_name": f"e2e {name} {uuid.uuid4().hex[:6]}", "principal_type": "SERVICE_ACCOUNT"}
        s, _, b = call("POST", f"{IDENTITY}/v1/principals", h, body)
        ok = check(f"create workload ({name}) is ACTIVE without a password", s == 201 and b.get("status") == "ACTIVE", f"HTTP {s} {brief(b)}")
        if ok:
            created[name] = b["principal_id"]
        if name == "survivor" and ok:
            s2, h2, b2 = call("POST", f"{IDENTITY}/v1/principals", h, body)
            check("create replayed with the same Idempotency-Key answers the first response",
                  s2 == 201 and b2.get("principal_id") == b["principal_id"], f"HTTP {s2} replayed={h2.get('Idempotent-Replayed')}")
    email = f"e2e-{uuid.uuid4().hex[:8]}@example.com"
    s, _, b = call("POST", f"{IDENTITY}/v1/principals", hdrs(sess["jwt"], write=True),
                   {"email": email, "display_name": "E2E Person", "principal_type": "HUMAN", "invite": False})
    if check("create person is PENDING_ACTIVATION", s == 201 and b.get("status") == "PENDING_ACTIVATION", f"HTTP {s} {brief(b)}"):
        created["person"] = b["principal_id"]
        s, _, rr = call("POST", f"{IDENTITY}/v1/principals/{created['person']}/password-resets", hdrs(sess["jwt"], write=True),
                        {"reason": "e2e: forgotten password"})
        if check("password reset requested (maker)", s == 202 and rr.get("request_id"), f"HTTP {s} {brief(rr)}"):
            s, _, b = call("POST", f"{IDENTITY}/v1/password-resets/{rr['request_id']}/decide", hdrs(sess["jwt"], write=True),
                           {"decision": "APPROVE"})
            check("reset requester cannot approve their own request", s in (401, 403), f"HTTP {s} {b.get('code')}")
    s, _, b = call("POST", f"{IDENTITY}/v1/account/password/forgot", {"X-Tenant-Id": TENANT}, {"email": "nobody-" + email})
    s2, _, b2 = call("POST", f"{IDENTITY}/v1/account/password/forgot", {"X-Tenant-Id": TENANT}, {"email": "admin@zoikosuite.com"})
    check("forgot-password answers unknown and known accounts identically", s == s2 == 202 and b.get("detail") == b2.get("detail"), f"HTTP {s}/{s2}")
    return created


# ── 5. step-up, merge and the authorization-svc consumer (IC-3) ──────────────

def merge(sess, created, password):
    if "survivor" not in created or "duplicate" not in created:
        check("merge: preconditions", False, "workload subjects were not created")
        return None
    s, _, b = call("POST", f"{IDENTITY}/v1/subjects/{created['survivor']}/merge",
                   hdrs(sess["jwt"], write=True, if_match='"v1"'), {"duplicate_id": created["duplicate"], "reason": "e2e"})
    check("merge without a second factor is refused (step_up_required)", s == 401 and b.get("code") == "step_up_required", f"HTTP {s} {b.get('code')}")

    s, _, enr = call("POST", f"{IDENTITY}/v1/mfa/totp/enroll", hdrs(sess["jwt"], write=True), {})
    if not check("TOTP enrolment", s in (200, 201) and enr.get("secret"), f"HTTP {s} {brief(enr)}"):
        return None
    s, _, b = call("POST", f"{IDENTITY}/v1/mfa/totp/activate", hdrs(sess["jwt"], write=True),
                   {"factor_id": enr["factor_id"], "code": totp(enr["secret"])})
    if not check("TOTP activation", s == 200, f"HTTP {s} {brief(b)}"):
        return None
    time.sleep(31 - time.time() % 30)  # a fresh step: the activation code is spent
    s, _, su = call("POST", f"{IDENTITY}/v1/authenticate/step-up", hdrs(sess["jwt"], write=True), {"code": totp(enr["secret"])})
    if not check("step-up with the second factor", s == 200 and su.get("access_token"), f"HTTP {s} {brief(su)}"):
        return enr
    s, b = resolve(su["access_token"])
    mfa_jwt = b.get("envelope_jwt")
    check("resolve after step-up: MFA-verified envelope", s in (200, 201) and mfa_jwt, f"HTTP {s}")
    sess["mfa_jwt"] = mfa_jwt

    s, hd, subj = call("GET", f"{IDENTITY}/v1/subjects/{created['duplicate']}", hdrs(mfa_jwt))
    etag = hd.get("ETag") if s == 200 else None
    check("subject identity read with ETag", s == 200 and etag, f"HTTP {s} etag={etag}")
    s, _, b = call("POST", f"{IDENTITY}/v1/subjects/{created['survivor']}/merge",
                   hdrs(mfa_jwt, write=True, if_match='"v999"'), {"duplicate_id": created["duplicate"], "reason": "e2e"})
    check("merge with a stale If-Match is refused (412)", s == 412, f"HTTP {s} {b.get('code')}")
    s, _, b = call("POST", f"{IDENTITY}/v1/subjects/{created['survivor']}/merge",
                   hdrs(mfa_jwt, write=True, if_match=etag), {"duplicate_id": created["duplicate"], "reason": "e2e duplicate workload"})
    if not check("merge with step-up and current ETag", s == 200 and b.get("merge_id"), f"HTTP {s} {brief(b)}"):
        return enr
    s, _, b = call("GET", f"{IDENTITY}/v1/subjects/{created['duplicate']}", hdrs(mfa_jwt))
    check("retired id resolves to its survivor", s == 200 and b.get("merged_into") == created["survivor"], f"HTTP {s} {brief(b)}")
    s, _, b = call("POST", f"{IDENTITY}/v1/subjects/{created['survivor']}/merge",
                   hdrs(mfa_jwt, write=True, if_match='"v2"'), {"duplicate_id": created["duplicate"], "reason": "again"})
    check("a retired id is never merged twice", s in (409, 412), f"HTTP {s} {b.get('code')}")
    out = psql("SELECT string_agg(event_type || ':' || CASE WHEN published_at IS NULL THEN 'pending' ELSE 'published' END, ',') "
               f"FROM event_outbox WHERE partition_key = '{created['duplicate']}' "
               "AND event_type IN ('identity.subject.merged','identity.subject.disabled')")
    check("outbox: identity.subject.merged + .disabled written", "identity.subject.merged" in out and "identity.subject.disabled" in out, out)
    # authorization-svc consumes the merge from Kafka (IC-3 consumer).
    t0, seen = time.time(), False
    while time.time() - t0 < 30 and not seen:
        logs = subprocess.run(["docker", "logs", "--since", "2m", "authorization-svc"], capture_output=True, text=True)
        seen = created["duplicate"] in (logs.stdout + logs.stderr) and "identity.subject.merged" in (logs.stdout + logs.stderr)
        if not seen:
            time.sleep(2)
    check("kafka → authorization-svc re-pointed the merged subject", seen, f"after {time.time() - t0:.1f}s")
    return enr


# ── 6. revocation reaches authorization-svc's introspection ──────────────────

def revocation(password):
    s, b = login(password)
    s, r = resolve(b.get("access_token", ""))
    jwt, sid = r.get("envelope_jwt"), r.get("session_context_id")
    if not jwt:
        check("revocation: second session", False, f"HTTP {s}")
        return
    s, _, _ = call("GET", f"{AUTHZ}/v1/admin/roles", hdrs(jwt))
    check("authz admin route admits the live envelope", s == 200, f"HTTP {s}")
    s, _, b = call("POST", f"{IDENTITY}/v1/context/session/{sid}/invalidate", hdrs(jwt, write=True), {"reason": "ADMIN_REVOKE"})
    check("session invalidated", s in (200, 204), f"HTTP {s} {brief(b)}")
    t0 = time.time()
    while True:
        s, _, _ = call("GET", f"{AUTHZ}/v1/admin/roles", hdrs(jwt))
        if s == 401 or time.time() - t0 > 60:
            break
        time.sleep(1)
    check("authz refuses the revoked envelope within the revocation TTL", s == 401, f"HTTP {s} after {time.time() - t0:.1f}s")


def undo(sess, enr, created):
    if enr and sess.get("mfa_jwt"):
        s, _, b = call("POST", f"{IDENTITY}/v1/mfa/totp/revoke", hdrs(sess["mfa_jwt"], write=True),
                       {"factor_id": enr["factor_id"], "reason_code": "E2E_CLEANUP"})
        check("undo: TOTP factor revoked", s in (200, 204), f"HTTP {s} {brief(b)}")
    if created.get("person"):
        s, hd, p = call("GET", f"{IDENTITY}/v1/principals/{created['person']}", hdrs(sess.get("mfa_jwt") or sess["jwt"]))
        s, _, b = call("PUT", f"{IDENTITY}/v1/principals/{created['person']}/status",
                       hdrs(sess.get("mfa_jwt") or sess["jwt"], write=True, if_match=hd.get("ETag")),
                       {"status": "DISABLED", "reason": "e2e cleanup"})
        check("undo: e2e person disabled", s in (200, 204), f"HTTP {s} {brief(b)}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--password", default=os.environ.get("E2E_ADMIN_PASSWORD", "Zoiko@Governance1"))
    a = ap.parse_args()
    health()
    sess = session(a.password)
    if not sess:
        return len([r for r in results if not r[1]])
    read_models(sess)
    authz_edge(sess)
    created = lifecycle(sess)
    enr = merge(sess, created, a.password)
    revocation(a.password)
    undo(sess, enr, created)
    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed")
    return len(failed)


if __name__ == "__main__":
    sys.exit(main())
