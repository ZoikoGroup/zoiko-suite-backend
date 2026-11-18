#!/usr/bin/env python3
"""IAM-8 load and revocation measurement for authorization-svc (RUNBOOK §13).

    python deployments/scripts/load-authorization.py [--concurrency 200] [--seconds 60]

1. Load: N workers call POST /v1/authorize as fast as they can for S seconds,
   with the persona mix of e2e-authorization-scenarios.py (PERMIT, DENY and
   limit-checked decisions — every call writes its decision artefact). Prints
   throughput and p50 / p95 / p99 / max latency, and every non-200.
2. Revocation lag: a fresh probe principal is granted a role, seen PERMITTED,
   the assignment is revoked through the admin API, and /v1/authorize is polled
   until it answers DENY. The lag is from the revoke's 200 to the first DENY.
   Run three times.

Run e2e-authorization-scenarios.py --seed-only first (the personas and their
roles must exist). Numbers are the local single-replica stack's, on a laptop
Docker VM: a measurement of this build here, not the production SLO.
"""
import argparse, json, statistics, sys, threading, time, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")
AUTHZ = "http://127.0.0.1:8089"
TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
CLERK = "a1000000-0000-4000-8000-000000000002"
APPROVER = "a1000000-0000-4000-8000-000000000003"

MIX = [
    (CLERK, "zt.payment.prepare", None),
    (CLERK, "zt.payment.release", None),
    (APPROVER, "zt.payment.release", {"amount": "900", "currency": "GBP", "quantity": "3"}),
    (APPROVER, "zt.payment.release", {"amount": "90000", "currency": "GBP", "quantity": "3"}),
    (APPROVER, "zt.invoice.view", None),
]


def hdrs(principal=ADMIN, write=False, if_match=None):
    rid = str(uuid.uuid4())
    h = {"X-Tenant-Id": TENANT, "X-Principal-Id": principal, "X-Legal-Entity-Id": ENTITY,
         "X-Correlation-ID": "zt-load-" + rid[:8], "X-Request-Id": rid, "X-Source-Channel": "api",
         "Content-Type": "application/json"}
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
    if if_match:
        h["If-Match"] = if_match
    return h


def post(path, body, principal=ADMIN, write=False, if_match=None, timeout=30):
    req = urllib.request.Request(AUTHZ + path, data=json.dumps(body).encode(), method="POST",
                                 headers=hdrs(principal, write, if_match))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"{}")
        except ValueError:
            return e.code, {}
    except (urllib.error.URLError, OSError) as e:
        return 0, {"error": str(e)}


def get(path, principal=ADMIN):
    req = urllib.request.Request(AUTHZ + path, headers=hdrs(principal))
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, {}


def decide(principal, action, attrs=None):
    body = {"principal_id": principal, "legal_entity_id": ENTITY, "action_type": action, "tenant_id": TENANT}
    if attrs:
        body["attributes"] = attrs
    return post("/v1/authorize", body)


def load(concurrency, seconds):
    lat, errors, lock = [], {}, threading.Lock()
    stop = time.time() + seconds

    def worker(i):
        n = i
        while time.time() < stop:
            p, a, attrs = MIX[n % len(MIX)]
            n += 1
            t0 = time.perf_counter()
            s, b = decide(p, a, attrs)
            dt = (time.perf_counter() - t0) * 1000
            with lock:
                if s == 200:
                    lat.append(dt)
                else:
                    k = f"{s} {b.get('error', b.get('code', ''))}"
                    errors[k] = errors.get(k, 0) + 1

    threads = [threading.Thread(target=worker, args=(i,), daemon=True) for i in range(concurrency)]
    t0 = time.time()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    wall = time.time() - t0
    lat.sort()
    q = lambda p: lat[min(len(lat) - 1, int(p * len(lat)))] if lat else float("nan")
    print(f"load: concurrency={concurrency} seconds={seconds} ok={len(lat)} errors={sum(errors.values())} "
          f"throughput={len(lat) / wall:.1f}/s")
    print(f"latency ms: p50={q(.50):.1f} p95={q(.95):.1f} p99={q(.99):.1f} max={lat[-1] if lat else float('nan'):.1f} "
          f"mean={statistics.mean(lat) if lat else float('nan'):.1f}")
    for k, v in sorted(errors.items(), key=lambda kv: -kv[1]):
        print(f"  non-200: {k} x{v}")
    return {"ok": len(lat), "errors": errors, "p50": q(.50), "p95": q(.95), "p99": q(.99)}


def revocation_lag(role_id):
    probe = "a1000000-0000-4000-8000-" + uuid.uuid4().hex[:12]
    s, a = post("/v1/admin/role-assignments", {"principal_id": probe, "role_id": role_id, "legal_entity_id": ENTITY,
                "effective_from": (datetime.now(timezone.utc) - timedelta(minutes=1)).isoformat(),
                "approval_reference": "ZT-REVOCATION-PROBE"}, write=True)
    if s not in (200, 201):
        print(f"revocation: grant failed HTTP {s} {a}")
        return None
    deadline = time.time() + 10
    while decide(probe, "zt.invoice.view")[1].get("decision") != "PERMIT":
        if time.time() > deadline:
            print("revocation: probe never permitted")
            return None
        time.sleep(0.05)
    s, b = post(f"/v1/admin/role-assignments/{a['principal_role_assignment_id']}/revoke",
                {"reason_code": "REVOCATION_PROBE", "purpose": "IAM-8 measurement"}, write=True, if_match=f'"v{a["version"]}"')
    t_revoked = time.perf_counter()
    if s != 200:
        print(f"revocation: revoke failed HTTP {s} {b}")
        return None
    polls = 0
    while True:
        polls += 1
        if decide(probe, "zt.invoice.view")[1].get("decision") == "DENY":
            lag = (time.perf_counter() - t_revoked) * 1000
            print(f"revocation: denied {lag:.0f} ms after the revoke ({polls} polls)")
            return lag
        if time.perf_counter() - t_revoked > 30:
            print("revocation: still permitted after 30 s")
            return None
        time.sleep(0.02)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--concurrency", type=int, default=200)
    ap.add_argument("--seconds", type=int, default=60)
    a = ap.parse_args()
    s, roles = get("/v1/admin/roles")
    role_id = next((r["role_id"] for r in (roles if isinstance(roles, list) else []) if r.get("role_code") == "ZT_INVOICE_VIEWER"), None)
    if not role_id:
        print("ZT_INVOICE_VIEWER missing: run e2e-authorization-scenarios.py --seed-only first")
        return 1
    res = load(a.concurrency, a.seconds)
    lags = [x for x in (revocation_lag(role_id) for _ in range(3)) if x is not None]
    if lags:
        print(f"revocation lag ms: runs={len(lags)} max={max(lags):.0f} mean={statistics.mean(lags):.0f}")
    return 0 if res["ok"] and lags else 1


if __name__ == "__main__":
    sys.exit(main())
