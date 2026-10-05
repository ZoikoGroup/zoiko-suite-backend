#!/usr/bin/env python3
"""Live proof, through a REAL Kafka, that authorization-svc honours what
delegated-authority-svc decides: delegation, extension, suspension, resume,
revocation and the delegation's own ceiling — plus the GOV-04 maker-checker
endpoint identity-context-svc calls.

Unit and DB tests cover each side; only this proves the outbox → topic →
consumer → projection → /v1/authorize chain end to end.

Needs: Kafka on localhost:9092 with the topic created; authorization-svc on
--authz consuming zoiko.delegated-authority.events, its DB seeded with
scripts/live_seed_authz.sql; delegated-authority-svc on --base publishing to
that topic. Exit code = number of failures.
"""
import argparse, json, sys, time, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:18136")
ap.add_argument("--authz", default="http://localhost:18089")
args = ap.parse_args()
sys.stdout.reconfigure(encoding="utf-8")
T, E = "7a000000-0000-4000-8000-000000000001", "7e000000-0000-4000-8000-000000000001"
fails = passes = 0


def check(name, ok, detail=""):
    global fails, passes
    passes, fails = (passes + 1, fails) if ok else (passes, fails + 1)
    print(("PASS  " if ok else "FAIL  ") + name + ("" if ok else f"  {detail}"))


def call(base, method, path, principal, body=None, write=True):
    r = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(), method=method)
    for k, v in {"Content-Type": "application/json", "X-Tenant-Id": T, "X-Principal-Id": principal, "X-Legal-Entity-Id": E,
                 "X-Request-Id": uuid.uuid4().hex, "X-Correlation-ID": uuid.uuid4().hex, "X-Source-Channel": "api"}.items():
        r.add_header(k, v)
    if write:
        r.add_header("Idempotency-Key", uuid.uuid4().hex)
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            return resp.status, json.loads(resp.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def authorize(principal, action, attrs=None):
    b = {"principal_id": principal, "legal_entity_id": E, "action_type": action}
    if attrs:
        b["attributes"] = attrs
    s, d = call(args.authz, "POST", "/v1/authorize", principal, b, write=False)
    return d.get("decision_outcome"), d.get("decision_basis", "")


def until(pred, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        v = pred()
        if v:
            return v
        time.sleep(0.5)
    return pred()


iso = lambda d: d.isoformat().replace("+00:00", "Z")
now = datetime.now(timezone.utc)
bob = "bob-" + uuid.uuid4().hex[:6]  # fresh delegate: no prior projection

print("── delegation → Kafka → authorization-svc projection")
check("before any delegation, bob may not approve payments", authorize(bob, "PAYMENT_APPROVE")[0] == "DENIED")
s, g = call(args.base, "POST", "/v1/delegations/", "alice", {
    "legal_entity_id": E, "delegator_principal_id": "alice", "delegate_principal_id": bob, "action_type": "PAYMENT_APPROVE",
    "effective_from": iso(now - timedelta(minutes=1)), "effective_to": iso(now + timedelta(hours=1)),
    "correlation_id": uuid.uuid4().hex, "reason": "kafka live check", "authority_limit_cents": 40000, "authority_limit_currency": "USD"})
check("alice delegates PAYMENT_APPROVE to bob, capped at 400.00 USD", s == 201 and g.get("status") == "ACTIVE", (s, g))
gid = g.get("delegation_id")
granted = until(lambda: authorize(bob, "PAYMENT_APPROVE", {"amount": "300", "currency": "USD"})[0] == "GRANTED")
check("the projection arrives: 300 USD under bob's delegation is GRANTED", granted)
out, basis = authorize(bob, "PAYMENT_APPROVE", {"amount": "450", "currency": "USD"})
check("450 USD exceeds the DELEGATION's ceiling and is DENIED", out == "DENIED" and "delegation_limit" in basis, (out, basis))
out, basis = authorize(bob, "PAYMENT_APPROVE", {"amount": "350", "currency": "GBP", "fx_rate": "0.0001"})
check("350 GBP (~449 USD at reference rates) is over the 400 USD cap; a caller fx_rate=0.0001 cannot shrink it",
      out == "DENIED" and "delegation_limit" in basis, (out, basis))

print("── extension, suspension, resume, revocation reach the decision")
newto = iso(now + timedelta(hours=5))
s, d = call(args.base, "POST", f"/v1/delegations/{gid}/extend?expected_version=1", "alice",
            {"new_effective_to": newto, "correlation_id": uuid.uuid4().hex, "reason": "longer cover"})
check("extend", s == 200, (s, d))
s, d = call(args.base, "POST", f"/v1/delegations/{gid}/suspend?expected_version=2", "alice", {"reason": "investigation"})
check("suspend", s == 200, (s, d))
check("a suspended delegation is DENIED at decision time",
      until(lambda: authorize(bob, "PAYMENT_APPROVE", {"amount": "10", "currency": "USD"})[0] == "DENIED"))
s, d = call(args.base, "POST", f"/v1/delegations/{gid}/resume?expected_version=3", "alice")
check("resume", s == 200, (s, d))
check("a resumed delegation is GRANTED again",
      until(lambda: authorize(bob, "PAYMENT_APPROVE", {"amount": "10", "currency": "USD"})[0] == "GRANTED"))
s, d = call(args.base, "POST", f"/v1/delegations/{gid}/revoke?expected_version=4", "alice", {"reason": "done"})
check("revoke", s == 200, (s, d))
check("a revoked delegation is DENIED",
      until(lambda: authorize(bob, "PAYMENT_APPROVE", {"amount": "10", "currency": "USD"})[0] == "DENIED"))

print("── GOV-04 maker-checker: /v1/sod/evaluate as identity-context-svc calls it")


def sod(maker, checker="", subject="", action="ATTACH_SUPPORT_CONTEXT"):
    r = urllib.request.Request(args.authz + "/v1/sod/evaluate", method="POST", data=json.dumps({
        "tenant_id": T, "action_type": action, "maker_principal_id": maker,
        "checker_principal_id": checker, "subject_principal_id": subject}).encode())
    # Exactly the headers identity-context-svc's client now sends.
    for k, v in {"Content-Type": "application/json", "X-Tenant-Id": T, "X-Principal-Id": maker,
                 "X-Source-Channel": "system", "X-Request-Id": "sod-live", "X-Correlation-ID": "sod-live"}.items():
        r.add_header(k, v)
    with urllib.request.urlopen(r, timeout=10) as resp:
        return json.loads(resp.read())


check("distinct maker, checker, subject → NO_CONFLICT", sod("alice", "admin2", "support-1")["result"] == "NO_CONFLICT")
check("maker approves own act → CONFLICT", sod("alice", "alice", "support-1").get("conflict_id") == "maker_is_checker")
check("suspending yourself → CONFLICT", sod("alice", "", "alice", "UPDATE_PRINCIPAL_STATUS").get("conflict_id") == "maker_is_subject")
check("checker holding a conflicting duty → CONFLICT (carol holds PAYMENT_RELEASE)",
      sod("alice", "carol", "support-1", "PAYMENT_APPROVE").get("conflict_id") == "held_duty_conflict")

print(f"\n{passes} passed, {fails} failed")
sys.exit(fails)
