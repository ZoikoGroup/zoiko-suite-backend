#!/usr/bin/env python3
"""Live proof of the 5 Oct 2026 ORG-06 remediation against a REAL authorization-svc.

The 28 Sep remediation passed every unit test and broke every create in a
wired stack: its SoD client called a route authorization-svc does not serve,
and only stubs ever answered it. This drives delegated-authority-svc and a real
authorization-svc end to end, so a contract mismatch cannot hide behind stubs.

Needs (see the header of each step for why):
  * authorization-svc on --authz, its DB seeded with scripts/live_seed_authz.sql
  * delegated-authority-svc on --base, AUTHZ_SERVICE_URL pointing at --authz,
    database at migration 000010
  * docker exec access to zoiko-postgres for the database facts

    python scripts/live_lifecycle_check.py [--base URL] [--authz URL]
Exit code = number of failures.
"""
import argparse, json, subprocess, sys, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:18136")
ap.add_argument("--db", default="delegated_authority")
args = ap.parse_args()
sys.stdout.reconfigure(encoding="utf-8")

T = "7a000000-0000-4000-8000-000000000001"
E = "7e000000-0000-4000-8000-000000000001"
RUN = uuid.uuid4().hex[:6]
# Delegates are unique per run. Grants last two days, so fixed names collided
# with a previous run's still-ACTIVE grants and the (correct) overlap rule
# refused them, which read as a regression. carol stays fixed: the seeded SoD
# rule depends on her role.
BOB, DAVE = "bob-" + RUN, "dave-" + RUN
fails = passes = 0


def check(name, ok, detail=""):
    global fails, passes
    if ok:
        passes += 1
        print(f"PASS  {name}")
    else:
        fails += 1
        print(f"FAIL  {name}  {detail}")


def req(method, path, principal, body=None, key=None):
    data = None if body is None else json.dumps(body, default=str).encode()
    r = urllib.request.Request(args.base + path, data=data, method=method)
    for k, v in {"Content-Type": "application/json", "X-Tenant-Id": T, "X-Principal-Id": principal,
                 "X-Legal-Entity-Id": E, "X-Request-Id": uuid.uuid4().hex, "X-Correlation-ID": "live-" + RUN,
                 "X-Source-Channel": "api"}.items():
        r.add_header(k, v)
    if method != "GET":
        r.add_header("Idempotency-Key", key or uuid.uuid4().hex)
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            return resp.status, json.loads(resp.read() or b"null"), dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except Exception:
            return e.code, {"raw": raw.decode(errors="replace")}, dict(e.headers)


def sql(q):
    out = subprocess.run(["docker", "exec", "zoiko-postgres", "psql", "-U", "postgres", "-d", args.db, "-tAc", q],
                         capture_output=True, text=True)
    return out.stdout.strip(), out.stderr.strip()


now = datetime.now(timezone.utc)
iso = lambda d: d.isoformat().replace("+00:00", "Z")


def grant(delegator, delegate, action, caller, **extra):
    b = {"legal_entity_id": E, "delegator_principal_id": delegator, "delegate_principal_id": delegate,
         "action_type": action, "effective_from": iso(now), "effective_to": iso(now + timedelta(days=2)),
         "correlation_id": f"{RUN}-{uuid.uuid4().hex[:6]}", "reason": "live check"}
    b.update(extra)
    return req("POST", "/v1/delegations/", caller, b)


print(f"run {RUN} against {args.base}")
print("── the regression: create through the REAL SoD engine")
s, d, _ = grant("alice", BOB, "PO_ISSUE", "alice")
check("delegator's own grant is created ACTIVE (SoD answered by the real /v1/sod/validate)",
      s == 201 and d.get("status") == "ACTIVE" and d.get("approval_method") == "DELEGATOR_SELF", (s, d))
gid = d.get("delegation_id")

s, d, _ = grant("alice", "carol", "PAYMENT_APPROVE", "alice")
check("SoD: carol holds PAYMENT_RELEASE, so PAYMENT_APPROVE is refused", s == 403 and d.get("error_code") == "sod_conflict", (s, d))

print("── ORG-06 negative case 11: the delegator's own limit (500.00 USD)")
s, d, _ = grant("alice", BOB, "PAYMENT_APPROVE", "alice", authority_limit_cents=90000, authority_limit_currency="USD")
check("a 900.00 USD ceiling above alice's 500.00 is refused", s == 403 and d.get("error_code") == "delegator_exceeds_limit", (s, d))
s, d, _ = grant("alice", BOB, "PAYMENT_APPROVE", "alice", authority_limit_cents=40000, authority_limit_currency="USD")
check("a 400.00 USD ceiling within alice's own is granted", s == 201 and d.get("authority_limit_cents") == 40000, (s, d))

print("── extend, suspend, resume, revoke — versions and reasons")
s, d, _ = req("POST", f"/v1/delegations/{gid}/extend?expected_version=1", "alice",
              {"new_effective_to": iso(now + timedelta(days=5)), "correlation_id": uuid.uuid4().hex, "reason": "leave extended"})
check("extend with a fresh correlation id succeeds (it used to be 409 every time)", s == 200 and d.get("version") == 2, (s, d))
s, d, _ = req("POST", f"/v1/delegations/{gid}/suspend?expected_version=1", "alice", {"reason": "x"})
check("a stale version is refused (409 version_mismatch)", s == 409 and d.get("error_code") == "version_mismatch", (s, d))
s, d, _ = req("POST", f"/v1/delegations/{gid}/suspend", "alice", {"reason": "x"})
check("no version is 428", s == 428, (s, d))
s, d, _ = req("POST", f"/v1/delegations/{gid}/suspend?expected_version=2", "alice", {"reason": "investigation"})
check("suspend", s == 200 and d.get("status") == "SUSPENDED" and d.get("suspension_reason") == "investigation", (s, d))
s, d, _ = req("POST", f"/v1/delegations/{gid}/resume?expected_version=3", BOB)
check("the delegate cannot resume their own grant", s == 403, (s, d))
s, d, _ = req("POST", f"/v1/delegations/{gid}/resume?expected_version=3", "alice")
check("resume (re-checked against the real authorization-svc)", s == 200 and d.get("status") == "ACTIVE", (s, d))
key = uuid.uuid4().hex
s, d, _ = req("POST", f"/v1/delegations/{gid}/revoke?expected_version=4", "alice", {"reason": "no longer needed"}, key=key)
check("revoke records the reason", s == 200 and d.get("revocation_reason") == "no longer needed", (s, d))
s, d, h = req("POST", f"/v1/delegations/{gid}/revoke?expected_version=4", "alice", {"reason": "no longer needed"}, key=key)
check("Idempotency-Key replays the first answer (cross-service finding 2)", s == 200 and h.get("X-Idempotent-Replay") == "true", (s, h.get("X-Idempotent-Replay")))

print("── Proposed → Active: maker-checker")
s, d, _ = grant("alice", DAVE, "PO_ISSUE", "admin2")
pid = d.get("delegation_id")
check("an administrator's grant on alice's behalf is only PROPOSED", s == 201 and d.get("status") == "PROPOSED", (s, d))
s, d, _ = req("POST", f"/v1/delegations/{pid}/activate?expected_version=1", "admin2")
check("the maker cannot approve their own proposal", s == 403 and d.get("error_code") == "approval_not_segregated", (s, d))
s, d, _ = req("POST", f"/v1/delegations/{pid}/activate?expected_version=1", "alice")
check("the delegator approves it", s == 200 and d.get("status") == "ACTIVE" and d.get("approval_method") == "DELEGATOR_APPROVAL", (s, d))
s, d, _ = req("POST", f"/v1/delegations/{pid}/extend?expected_version=2", "admin2",
              {"new_effective_to": iso(now + timedelta(days=4)), "correlation_id": uuid.uuid4().hex, "reason": "longer"})
check("its maker cannot then extend it alone (self-approval, 6 Oct)", s == 403 and d.get("error_code") == "approval_not_segregated", (s, d))

print("── re-audit 6 Oct (second pass)")
corr = f"{RUN}-reuse"
body = {"legal_entity_id": E, "delegator_principal_id": "alice", "action_type": "PO_ISSUE",
        "effective_from": iso(now), "effective_to": iso(now + timedelta(days=1)), "correlation_id": corr, "reason": "reuse probe"}
s, d, _ = req("POST", "/v1/delegations/", "alice", dict(body, delegate_principal_id="erin-" + RUN))
check("a grant under a fresh correlation_id", s == 201, (s, d))
s, d, _ = req("POST", "/v1/delegations/", "alice", dict(body, delegate_principal_id="frank-" + RUN))
check("the same correlation_id naming another delegate is 409 correlation_reused, not a replay of erin's grant",
      s == 409 and d.get("error_code") == "correlation_reused", (s, d))
s, d, _ = grant("admin2", "gina-" + RUN, "PAYMENT_APPROVE", "admin2", authority_limit_cents=10000, authority_limit_currency="USD")
check("admin2 (no authority limit of their own) may delegate a NARROWER 100.00 USD ceiling",
      s == 201 and d.get("status") == "ACTIVE", (s, d))
s, d, _ = req("GET", f"/v1/delegations/{gid}", "mallory-" + RUN)
s2, d2, _ = req("GET", f"/v1/delegations/{uuid.uuid4()}", "mallory-" + RUN)
check("a refused read is indistinguishable from a missing id", s == 404 and (s, d) == (s2, d2), ((s, d), (s2, d2)))

print("── what the database recorded")
out, _ = sql(f"SELECT string_agg(transition, ',' ORDER BY version) FROM delegation_history WHERE delegation_id = '{gid}'")
check("append-only history of every transition", out == "ACTIVATED,EXTENDED,SUSPENDED,RESUMED,REVOKED", out)
out, _ = sql(f"SELECT string_agg(event_type, ',' ORDER BY outbox_id) FROM delegation_outbox WHERE delegation_id = '{gid}'")
check("each transition's event is in the outbox (authority.extended could not be written before)",
      out == "authority.delegated,authority.extended,authority.suspended,authority.resumed,authority.revoked", out)
out, _ = sql(f"SELECT string_agg(refusal_reason, ',' ORDER BY refused_at) FROM refused_escalations WHERE tenant_id = '{T}' AND refused_at > now() - interval '5 minutes'")
check("refused escalations are durable evidence", all(r in (out or "") for r in ("sod_conflict", "delegator_exceeds_limit", "approval_not_segregated")), out)
_, err = sql(f"DELETE FROM delegation_history WHERE delegation_id = '{gid}'")
check("history refuses DELETE", "append-only" in err, err)

print(f"\n{passes} passed, {fails} failed")
sys.exit(fails)
