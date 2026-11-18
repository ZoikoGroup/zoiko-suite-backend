#!/usr/bin/env python3
"""End-to-end check of governance-decision-log-svc and every service it depends on.

Each check crosses one real dependency edge on the local compose stack, so a
failure names the edge that is broken:

  postgres                  migrations through 000016: hash-chain trigger, RLS
                            forced on every table, runtime role not a superuser
  authorization-svc         GOVERNANCE_DECISION_RECORD / _READ / _REPLAY and
                            GOVERNANCE_EVIDENCE_SEAL granted to the demo
                            principal; a principal without READ is refused (403)
  kafka                     governance.decision.recorded relayed from the outbox
                            and readable on zoiko.governance.events
  policy-svc                a policy evaluation is written into the ledger by
                            policy-svc's relay (as svc-policy); a replay
                            re-fetches the exact policy version (manifest)
  the ledger itself         record / idempotent repeat / conflict, register
                            fields, follow-ups, filters, cursor paging, summary,
                            evidence chain verify, seal, package verify, and an
                            OFFLINE re-verification of the exported bundle
                            (every link and every Merkle root recomputed here,
                            in Python, from the bundle alone)

    python deployments/scripts/e2e-governance-decision-log-deps.py [--no-policy]

Run seed-demo-rbac.ps1 first (the demo principal needs the GOVERNANCE_FULL
bundle; svc-policy needs GOVERNANCE_DECISION_RECORD). The ledger is append-only:
what this writes stays, by design. --no-policy skips the policy-svc edge and
reports it as SKIPPED (a skip is never a pass).

Exit status is the number of failed checks.
"""
import argparse, base64, hashlib, http.client, json, os, ssl, subprocess, sys, tempfile, time, urllib.error, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")

TENANT = "11111111-1111-1111-1111-111111111111"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
APPROVER = "66666666-6666-6666-6666-666666666666"
PLATFORM = "00000000-0000-0000-0000-00000000f001"
STRANGER = "e2e00000-0000-4000-8000-0000000000aa"

H = "http://127.0.0.1"
GDL, AUTHZ, POLICY = f"{H}:8083", f"{H}:8089", f"{H}:8085"
PG = "zoiko-postgres"
KAFKA = "zoiko-kafka"
TOPIC = "zoiko.governance.events"
RUN = uuid.uuid4().hex[:8]

results = []


def check(name, ok, detail=""):
    results.append((name, "PASS" if ok else "FAIL", detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


def skip(name, why):
    results.append((name, "SKIP", why))
    print(f"SKIP  {name}  — {why}", flush=True)


def envelope(principal=ADMIN, write=False, tenant=TENANT, entity=ENTITY, purpose="GOVERNANCE_AUDIT"):
    rid = str(uuid.uuid4())
    h = {
        "X-Tenant-Id": tenant,
        "X-Principal-Id": principal,
        "X-Legal-Entity-Id": entity,
        "X-Correlation-ID": f"e2e-gdl-{RUN}-{rid[:8]}",
        "X-Request-Id": rid,
        "X-Source-Channel": "api",
        "X-Source-System": "e2e-governance-decision-log-deps",
    }
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
    else:
        h["X-Purpose-Context"] = purpose
    return h


def call(method, url, headers=None, body=None, timeout=20, raw=False):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            status, hdrs, content = r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        status, hdrs, content = e.code, e.headers, e.read()
    except (urllib.error.URLError, OSError) as e:
        return 0, {}, {"error": f"unreachable: {e}"}
    if raw:
        return status, hdrs, content
    try:
        parsed = json.loads(content) if content else {}
    except ValueError:
        parsed = {"raw": content[:200].decode("utf8", "replace")}
    return status, hdrs, parsed


_ADMIN_JWT = {"jwt": None, "at": 0.0}
_PEER = {}


def admin_jwt():
    """The demo admin's signed envelope, renewed before its 5-minute expiry."""
    if not _ADMIN_JWT["jwt"] or time.time() - _ADMIN_JWT["at"] > 200:
        jwt, why = envelope_jwt()
        if not jwt:
            raise RuntimeError("no identity envelope: " + why)
        _ADMIN_JWT.update(jwt=jwt, at=time.time())
    return _ADMIN_JWT["jwt"]


def peer_cert():
    """policy-svc's client certificate: a registered peer of the ledger."""
    if "paths" not in _PEER:
        paths, why = provision_cert("policy-svc")
        if not paths:
            raise RuntimeError("no peer certificate: " + why)
        _PEER["paths"] = paths
    return _PEER["paths"]


def gdl(method, path, principal=ADMIN, body=None, headers=None, tenant=TENANT, raw=False, **kw):
    """A ledger call. The demo admin presents its verified envelope (the
    console's path); any other principal is vouched for by policy-svc over
    mTLS (a service relay's path), so its own grants are what is tested."""
    h = envelope(principal, write=method != "GET", tenant=tenant)
    h.update(headers or {})
    if principal == ADMIN and tenant == TENANT:
        h["Authorization"] = "Bearer " + admin_jwt()
        return call(method, GDL + path, h, body, raw=raw, **kw)
    s, hd, b = mtls_call(peer_cert(), method, path, h, body)
    return s, hd, (json.dumps(b).encode() if raw else b)


def until(predicate, seconds=30, step=1.0):
    deadline = time.time() + seconds
    while True:
        v = predicate()
        if v[0] or time.time() >= deadline:
            return v
        time.sleep(step)


def psql(sql, db="governance_decision_log"):
    r = subprocess.run(["docker", "exec", "-i", PG, "psql", "-X", "-q", "-U", "postgres", "-d", db, "-tA", "-c", sql],
                       capture_output=True)
    return r.returncode, r.stdout.decode("utf8", "replace").strip(), r.stderr.decode("utf8", "replace").strip()


def brief(body):
    return json.dumps(body)[:240]


def decision_body(**over):
    b = {
        "decision_id": f"e2e-{RUN}-{uuid.uuid4().hex[:10]}",
        "legal_entity_id": ENTITY,
        "action_type": "VENDOR_CONTRACT_RENEWAL",
        "outcome": "PENDING",
        "rule_basis": "PROCUREMENT-POLICY:v2.1",
        "title": "Approve vendor contract renewal",
        "summary": "Renewal of cloud infrastructure services contract with strategic vendor.",
        "priority": "HIGH",
        "tags": ["Vendor", "Contract", "Renewal"],
        "related_obligation_id": "OB-2026-1123",
        "action_subject_type": "CONTRACT",
        "action_subject_id": "CON-2026-7890",
        "jurisdiction_context": "GB",
    }
    b.update(over)
    return b


# ── 0. health and schema ─────────────────────────────────────────────────────

def health():
    for name, url in [("governance-decision-log-svc /healthz", f"{GDL}/healthz"),
                      ("governance-decision-log-svc /readyz", f"{GDL}/readyz"),
                      ("authorization-svc /readyz", f"{AUTHZ}/readyz")]:
        s, _, _ = call("GET", url, timeout=5)
        check(f"health {name}", s == 200, f"HTTP {s}")


def schema():
    rc, out, err = psql("SELECT count(*) FROM schema_migrations WHERE filename = '000016_evidence_ledger.up.sql'")
    check("postgres: 000016_evidence_ledger applied", rc == 0 and out == "1", out or err)
    rc, out, _ = psql("SELECT string_agg(tgname, ',' ORDER BY tgname) FROM pg_trigger WHERE tgrelid = 'governance_decisions'::regclass AND NOT tgisinternal")
    check("postgres: classify + chain + append-only triggers on governance_decisions",
          all(t in out for t in ("gdl_10_classify", "gdl_20_chain", "governance_decisions_immutable")) and "truncate" in out, out)
    rc, out, _ = psql("SELECT string_agg(relname || '=' || relforcerowsecurity, ',' ORDER BY relname) FROM pg_class "
                      "WHERE relname IN ('governance_decisions','replay_manifests','idempotency_keys','outbox','evidence_packages')")
    check("postgres: FORCE row-level security on all five tables", out.count("=t") == 5, out)
    rc, out, _ = psql("SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = 'app_governance_decision_log'")
    check("postgres: runtime role is neither superuser nor BYPASSRLS", out == "f", out)


# ── 1. authorization-svc edge ────────────────────────────────────────────────

def authz_grants():
    for action in ("GOVERNANCE_DECISION_RECORD", "GOVERNANCE_DECISION_READ", "GOVERNANCE_DECISION_REPLAY", "GOVERNANCE_EVIDENCE_SEAL"):
        s, _, b = call("POST", f"{AUTHZ}/v1/authorize", envelope(ADMIN, entity=PLATFORM),
                       {"principal_id": ADMIN, "legal_entity_id": PLATFORM, "action_type": action})
        check(f"authz: demo principal holds {action} on the platform scope", s == 200 and b.get("decision") == "PERMIT",
              f"HTTP {s} {b.get('decision')} {b.get('decision_basis', b.get('error', ''))}")

    s, _, b = gdl("GET", "/v1/decisions?limit=1", principal=STRANGER)
    check("read authz enforced: principal without GOVERNANCE_DECISION_READ → 403", s == 403, f"HTTP {s} {b.get('code')}")
    h = envelope(ADMIN)
    del h["X-Principal-Id"]
    s, _, b = call("GET", f"{GDL}/v1/decisions?limit=1", h)
    check("read without a principal → 401", s == 401, f"HTTP {s} {b.get('code')}")
    s, _, b = gdl("POST", "/v1/decisions", principal=STRANGER, body=decision_body())
    check("write authz: principal without GOVERNANCE_DECISION_RECORD → 403", s == 403, f"HTTP {s} {b.get('code')}")


# ── 2. the ledger: record, register, follow-up, filters, summary ─────────────

def ledger():
    state = {}
    before_s, _, before = gdl("GET", "/v1/decisions/summary?days=30")
    check("summary readable", before_s == 200, f"HTTP {before_s} {brief(before)}")
    before_total = before.get("totals", {}).get("total", 0) if before_s == 200 else 0

    body = decision_body()
    hdr = envelope(ADMIN, write=True)
    hdr["Authorization"] = "Bearer " + admin_jwt()
    s, _, d = call("POST", f"{GDL}/v1/decisions", hdr, body)
    ok = check("record: POST /v1/decisions → 201", s == 201, f"HTTP {s} {brief(d)}")
    if not ok:
        return state
    state["parent"] = d
    check("record: chain link assigned by the database",
          d.get("chain_seq", 0) >= 1 and len(d.get("curr_hash", "")) == 64 and len(d.get("prev_hash", "")) == 64,
          f"seq {d.get('chain_seq')} ref {d.get('display_ref')}")
    check("record: register fields stored", d.get("title") == body["title"] and d.get("priority") == "HIGH"
          and d.get("tags") == body["tags"] and d.get("related_obligation_id") == "OB-2026-1123", brief(d))
    check("record: domain classified from action_type (CONTRACT → Legal & Contracts)", d.get("domain") == "LEGAL_CONTRACTS", d.get("domain"))
    check("record: status category from the outcome", d.get("status_category") == "PENDING", d.get("status_category"))
    check("record: actor is the authenticated principal", d.get("actor_id") == ADMIN, d.get("actor_id"))
    check("F11: retention class EV-REGULATED, record class COMPLIANCE_EVIDENCE on the decision",
          d.get("retention_class") == "EV-REGULATED" and d.get("record_class") == "COMPLIANCE_EVIDENCE",
          f"{d.get('retention_class')} / {d.get('record_class')}")
    state["correlation"] = hdr["X-Correlation-ID"]

    s, _, again = call("POST", f"{GDL}/v1/decisions", hdr, body)
    check("idempotent repeat: same key + body → 200 with the stored row", s == 200 and again.get("curr_hash") == d["curr_hash"], f"HTTP {s}")
    s, _, b = call("POST", f"{GDL}/v1/decisions", hdr, dict(body, outcome="DENIED"))
    check("same key, different body → 409 IDEMPOTENCY_MISMATCH", s == 409 and b.get("code") == "IDEMPOTENCY_MISMATCH", f"HTTP {s} {b.get('code')}")
    s, _, b = gdl("POST", "/v1/decisions", body=dict(body, outcome="DENIED"))
    check("same decision_id, different content → 409 DECISION_ID_CONFLICT", s == 409 and b.get("code") == "DECISION_ID_CONFLICT", f"HTTP {s} {b.get('code')}")
    s, _, b = gdl("POST", "/v1/decisions", body=decision_body(priority="URGENT"))
    check("invalid priority → 400", s == 400, f"HTTP {s} {b.get('detail')}")
    s, _, b = gdl("POST", "/v1/decisions", body=decision_body(domain="NOT_A_DOMAIN"))
    check("unknown domain → 400 UNKNOWN_DOMAIN", s == 400 and b.get("code") == "UNKNOWN_DOMAIN", f"HTTP {s} {b.get('code')}")

    s, _, f = gdl("POST", "/v1/decisions", body=decision_body(outcome="APPROVED", causation_id=d["decision_id"],
                                                              title="Resolve: Approve vendor contract renewal"))
    check("follow-up recorded with causation_id", s == 201 and f.get("causation_id") == d["decision_id"]
          and f.get("chain_seq") == d["chain_seq"] + 1 and f.get("prev_hash") == d["curr_hash"], f"HTTP {s} {brief(f)}")
    state["follow_up"] = f
    s, _, p = gdl("GET", f"/v1/decisions/{d['decision_id']}")
    check("parent shows follow_up_count=1", s == 200 and p.get("follow_up_count") == 1, f"HTTP {s} {p.get('follow_up_count')}")
    s, _, lst = gdl("GET", f"/v1/decisions?causation={d['decision_id']}")
    check("list ?causation= returns the follow-up", s == 200 and [r["decision_id"] for r in lst.get("records", [])] == [f.get("decision_id")],
          f"HTTP {s} total {lst.get('total_count')}")

    s, _, other = gdl("POST", "/v1/decisions", body=decision_body(action_type="DATA_ACCESS_EXCEPTION", outcome="ESCALATED",
                                                                  title="Data access exception", priority="MEDIUM",
                                                                  jurisdiction_context="EU", tags=[]))
    check("second decision classified Data & Privacy", s == 201 and other.get("domain") == "DATA_PRIVACY", f"HTTP {s} {other.get('domain')}")

    for q, want in [(f"domain=LEGAL_CONTRACTS&q={RUN}", 2), (f"status=ESCALATED&q={RUN}", 1), ("jurisdiction=EU&q=" + RUN, 1),
                    (f"priority=HIGH&q={RUN}", 2), (f"q={d['display_ref']}", 1)]:
        s, _, lst = gdl("GET", "/v1/decisions?" + q)
        check(f"list filter {q.split('&')[0]} → {want}", s == 200 and lst.get("total_count") == want, f"HTTP {s} total {lst.get('total_count')}")

    s, _, page1 = gdl("GET", f"/v1/decisions?q={RUN}&limit=2")
    s2, _, page2 = gdl("GET", f"/v1/decisions?q={RUN}&limit=2&cursor={page1.get('next_cursor', '')}") if page1.get("next_cursor") else (0, {}, {})
    ids = [r["decision_id"] for r in page1.get("records", []) + page2.get("records", [])]
    check("cursor paging: two pages, three distinct decisions, total_count 3",
          s == 200 and s2 == 200 and len(set(ids)) == 3 and page1.get("total_count") == 3, f"{len(ids)} ids, total {page1.get('total_count')}")

    s, _, lst = gdl("GET", f"/v1/decisions?correlation={state['correlation']}")
    check("F11: audit dimension: ?correlation= returns exactly the decision recorded under that trace",
          s == 200 and lst.get("total_count") == 1 and [r["decision_id"] for r in lst.get("records", [])] == [d["decision_id"]],
          f"HTTP {s} total {lst.get('total_count')}")
    s, _, b = gdl("GET", "/v1/decisions?correlation=" + "c" * 65)
    check("F11: over-long correlation → 400", s == 400, f"HTTP {s} {b.get('code')}")

    s, _, b = gdl("GET", f"/v1/decisions/{d['decision_id']}", principal=ADMIN, tenant="99999999-9999-9999-9999-999999999999")
    check("tenant isolation: another tenant gets no row (403/404)", s in (403, 404), f"HTTP {s}")

    s, _, after = gdl("GET", "/v1/decisions/summary?days=30")
    check("summary counts the new decisions", s == 200 and after["totals"]["total"] >= before_total + 3
          and any(x["key"] == "LEGAL_CONTRACTS" and x["count"] >= 2 for x in after.get("by_domain", []))
          and len(after.get("trend", [])) >= 30, f"total {before_total} → {after.get('totals', {}).get('total')}")
    check("summary: chain length covers the new decisions", after.get("chain", {}).get("length", 0) >= other.get("chain_seq", 1),
          brief(after.get("chain")))
    return state


# ── 3. kafka edge: the outbox relay ──────────────────────────────────────────

def kafka(state):
    d = state.get("parent")
    if not d:
        skip("kafka: governance.decision.recorded relayed", "no decision recorded")
        return

    def published():
        s, _, ev = gdl("GET", f"/v1/decisions/{d['decision_id']}/evidence")
        evs = ev.get("events", []) if s == 200 else []
        return (any(e.get("event_type") == "governance.decision.recorded" and e.get("published_at") for e in evs), ev)

    ok, ev = until(published, 45, 2)
    check("outbox: governance.decision.recorded published by the relay", ok, brief(ev.get("events")))
    check("evidence: the decision's own link re-computes", ev.get("hash_valid") is True and ev.get("prev_link_valid") is True, brief(ev))

    r = subprocess.run(["docker", "exec", KAFKA, "/opt/kafka/bin/kafka-console-consumer.sh", "--bootstrap-server", "localhost:9092",
                        "--topic", TOPIC, "--from-beginning", "--timeout-ms", "15000"], capture_output=True)
    out = r.stdout.decode("utf8", "replace")
    if r.returncode != 0 and not out:
        r = subprocess.run(["docker", "exec", KAFKA, "kafka-console-consumer", "--bootstrap-server", "localhost:9092",
                            "--topic", TOPIC, "--from-beginning", "--timeout-ms", "15000"], capture_output=True)
        out = r.stdout.decode("utf8", "replace")
    found = [line for line in out.splitlines() if d["decision_id"] in line]
    ok = check(f"kafka: event for the decision on {TOPIC}", bool(found), f"{len(out.splitlines())} messages read")
    if ok:
        env = json.loads(found[0])
        payload = env.get("payload") if isinstance(env.get("payload"), dict) else json.loads(base64.b64decode(env["payload"])) \
            if isinstance(env.get("payload"), str) else {}
        digest = "sha256:" + hashlib.sha256(json.dumps(env["payload"], separators=(",", ":")).encode()).hexdigest() \
            if isinstance(env.get("payload"), dict) else None
        check("kafka: envelope carries the chain link and domain",
              payload.get("curr_hash") == d["curr_hash"] and payload.get("domain") == "LEGAL_CONTRACTS", brief(payload))
        check("kafka: event type and aggregate", env.get("event_type") == "governance.decision.recorded"
              and env.get("aggregate_id") == d["decision_id"], brief(env)[:160])
        check("F11: event carries retention_class EV-REGULATED", env.get("retention_class") == "EV-REGULATED", env.get("retention_class"))
        # The follow-up's events name this decision as their causation, so
        # match on the aggregate, not on the id appearing anywhere.
        aliases = [e for e in (json.loads(line) for line in found)
                   if e.get("event_type") == "com.zoikosuite.governance.decision.recorded" and e.get("aggregate_id") == d["decision_id"]]
        check("C3/R9: canonical com.zoikosuite.governance.decision.recorded published alongside, same payload_hash, alias_of the wire event",
              len(aliases) == 1 and aliases[0].get("alias_of_event_id") == env.get("event_id")
              and aliases[0].get("payload_hash") == env.get("payload_hash") and aliases[0].get("event_id") != env.get("event_id"),
              f"{len(aliases)} alias(es) {brief(aliases[:1])[:160]}")


# ── 4. evidence ledger: verify, seal, package verify, offline bundle check ───

def merkle(leaves):
    def mth(hs):
        if len(hs) == 1:
            return hs[0]
        k = 1
        while k * 2 < len(hs):
            k *= 2
        return hashlib.sha256(b"\x01" + mth(hs[:k]) + mth(hs[k:])).digest()
    if not leaves:
        return hashlib.sha256(b"").digest()
    return mth([hashlib.sha256(b"\x00" + leaf).digest() for leaf in leaves])


def evidence():
    s, _, bundle_head = gdl("GET", "/v1/evidence/chain/export")
    check("F11: export bundle carries retention_class / record_class",
          s == 200 and bundle_head.get("retention_class") == "EV-REGULATED" and bundle_head.get("record_class") == "COMPLIANCE_EVIDENCE",
          f"HTTP {s} {bundle_head.get('retention_class')} {bundle_head.get('record_class')}")
    s, _, v = gdl("GET", "/v1/evidence/chain/verify")
    check("chain verify: every link and package re-computes", s == 200 and v.get("valid") is True,
          f"HTTP {s} checked {v.get('decisions_checked')} break {v.get('first_break')}")

    s, _, pkg = gdl("POST", "/v1/evidence/packages/seal", body={"label": f"e2e {RUN}", "reason": "dependency e2e"},
                    headers={"Idempotency-Key": f"e2e-seal-{RUN}"})
    check("seal: package sealed through the head", s == 201 and pkg.get("to_seq") == v.get("head_seq")
          and len(pkg.get("merkle_root", "")) == 64, f"HTTP {s} {brief(pkg)}")
    s2, _, again = gdl("POST", "/v1/evidence/packages/seal", body={"label": f"e2e {RUN}", "reason": "dependency e2e"},
                       headers={"Idempotency-Key": f"e2e-seal-{RUN}"})
    check("seal retry with the same key → 200, same package", s2 == 200 and again.get("package_id") == pkg.get("package_id"), f"HTTP {s2}")
    s3, _, b = gdl("POST", "/v1/evidence/packages/seal", body={})
    check("seal with nothing new → 409 NOTHING_TO_SEAL", s3 == 409 and b.get("code") == "NOTHING_TO_SEAL", f"HTTP {s3} {b.get('code')}")
    s4, _, b = gdl("POST", "/v1/evidence/packages/seal", principal=STRANGER, body={})
    check("seal without GOVERNANCE_EVIDENCE_SEAL → 403", s4 == 403, f"HTTP {s4} {b.get('code')}")
    if pkg.get("package_id"):
        s, _, c = gdl("GET", f"/v1/evidence/packages/{pkg['package_id']}/verify")
        check("package verify: Merkle root re-computes", s == 200 and c.get("valid") is True, f"HTTP {s} {brief(c)}")
        s, _, lst = gdl("GET", "/v1/evidence/packages")
        check("packages listed newest first", s == 200 and lst.get("records", [{}])[0].get("package_id") == pkg["package_id"], f"HTTP {s}")

    s, hdrs, raw = gdl("GET", "/v1/evidence/chain/export", raw=True, timeout=60)
    ok = check("export: evidence bundle downloads as an attachment", s == 200 and "attachment" in (hdrs.get("Content-Disposition") or ""),
               f"HTTP {s} {hdrs.get('Content-Disposition') if hdrs else ''}")
    if not ok:
        return
    bundle = json.loads(raw)
    prev = bytes(32)
    broken = None
    leaves = []
    for i, dec in enumerate(bundle["decisions"]):
        p, c = bytes.fromhex(dec["prev_hash"]), bytes.fromhex(dec["curr_hash"])
        if dec["chain_seq"] != i + 1 or p != prev or hashlib.sha256(p + dec["canonical"].encode()).digest() != c:
            broken = dec["chain_seq"]
            break
        leaves.append(c)
        prev = c
    check(f"offline: all {len(bundle['decisions'])} links re-computed from the bundle alone", broken is None, f"first break {broken}")
    prev_pkg = bytes(32)
    bad = []
    for p in bundle["packages"]:
        root = merkle(leaves[: p["to_seq"]])
        head = leaves[p["to_seq"] - 1]
        ph = hashlib.sha256(prev_pkg + root + head + p["to_seq"].to_bytes(8, "big")).digest()
        if root.hex() != p["merkle_root"] or ph.hex() != p["package_hash"] or prev_pkg.hex() != p["prev_package_hash"]:
            bad.append(p["package_seq"])
        prev_pkg = bytes.fromhex(p["package_hash"])
    check(f"offline: all {len(bundle['packages'])} package roots and package hashes re-computed (RFC 6962)", not bad, f"bad {bad}")


# ── 5. policy-svc edge: relay into the ledger, and replay ────────────────────

def policy():
    s, _, _ = call("GET", f"{POLICY}/readyz", timeout=5)
    if not check("health policy-svc /readyz", s == 200, f"HTTP {s}"):
        return

    def pol(method, path, principal=ADMIN, body=None, entity=ENTITY, if_match=None):
        h = envelope(principal, write=method != "GET", entity=entity, purpose="POLICY_REVIEW")
        if if_match:
            h["If-Match"] = if_match
        return call("POST" if method == "POST" else method, POLICY + path, h, body)

    # One threshold policy governs a scope at a time: a later run reuses the
    # version an earlier run activated instead of being refused POLICY_CONFLICTED.
    s, _, res = pol("GET", f"/v1/policies/resolve?policy_type=APPROVAL_THRESHOLD&tenant_id={TENANT}&legal_entity_id={ENTITY}")
    selected = res.get("selected") if s == 200 and res.get("status") == "RESOLVED" else None
    if selected and (selected.get("rule_payload") or {}).get("threshold_amount") is not None:
        vid = selected["policy_version_id"]
        threshold = float(selected["rule_payload"]["threshold_amount"])
        check("policy: an active APPROVAL_THRESHOLD version governs the tenant (reused)", True, f"{vid} threshold {threshold:g}")
        evaluate_and_replay(pol, vid, threshold)
        return

    code = f"E2E_GDL_THRESHOLD_{RUN.upper()}"
    s, _, p = pol("POST", "/v1/policies", body={"policy_code": code, "policy_name": "E2E approval threshold",
                                                "policy_type": "APPROVAL_THRESHOLD", "scope": "TENANT"})
    if not check("policy: threshold policy created", s in (200, 201), f"HTTP {s} {brief(p)}"):
        return
    pid = p.get("policy_id")
    s, _, v = pol("POST", f"/v1/policies/{pid}/versions", body={
        "tenant_id": TENANT, "rule_payload": {"threshold_amount": 10000},
        "effective_from": (datetime.now(timezone.utc) - timedelta(minutes=5)).isoformat(),
        "rationale": "e2e governance-decision-log", "source": "e2e"})
    if not check("policy: version drafted", s in (200, 201), f"HTTP {s} {brief(v)}"):
        return
    vid = v.get("policy_version_id")
    steps = [("submit-review", ADMIN, {"reason": "e2e"}), ("approve", APPROVER, {"reason": "e2e", "reason_code": "MEETS_POLICY", "outcome": "APPROVED"}),
             ("activate", APPROVER, {"reason": "e2e"})]
    for step, who, body in steps:
        # Every transition is optimistic-concurrency checked (API-3): the
        # version's current ETag goes in If-Match.
        _, vh, _ = pol("GET", f"/v1/policy-versions/{vid}")
        etag = vh.get("ETag") if vh else None
        s, _, b = pol("POST", f"/v1/policies/{pid}/versions/{vid}/{step}", principal=who, body=body, if_match=etag)
        if not check(f"policy: {step}", s in (200, 201), f"HTTP {s} {brief(b)}"):
            return

    evaluate_and_replay(pol, vid, 10000.0)


def evaluate_and_replay(pol, vid, threshold):
    """Evaluate above the threshold through policy-svc, wait for its relay to
    write the decision into the ledger, then replay it here."""
    amount = threshold + 5000
    decision_id = f"e2e-pol-{RUN}"
    s, _, ev = pol("POST", "/v1/policies/evaluate", body={"policy_type": "APPROVAL_THRESHOLD", "tenant_id": TENANT,
                                                          "legal_entity_id": ENTITY, "action_context": {"amount": amount},
                                                          "decision_id": decision_id})
    if not check(f"policy: evaluation ({amount:g} > {threshold:g} → escalated)", s in (200, 201), f"HTTP {s} {brief(ev)}"):
        return

    def relayed():
        s, _, d = gdl("GET", f"/v1/decisions/{decision_id}")
        return (s == 200, d)

    ok, d = until(relayed, 60, 2)
    check("policy relay: the evaluation is in the ledger, written by svc-policy", ok and d.get("actor_id") == "svc-policy",
          f"{d.get('actor_id')} {d.get('outcome')} {d.get('display_ref')}")
    if not ok:
        return
    check("policy relay: outcome and policy version recorded", d.get("outcome") == "ESCALATED" and d.get("policy_version_id") == vid,
          f"{d.get('outcome')} {d.get('policy_version_id')}")

    s, _, m = gdl("POST", f"/v1/decisions/{decision_id}/replay", body={})
    check("replay: re-evaluated against the exact policy version → manifest, reproduced",
          s == 201 and m.get("outcomes_match") is True and m.get("policy_version_id") == vid, f"HTTP {s} {brief(m)}")
    s, _, ms = gdl("GET", f"/v1/decisions/{decision_id}/replay-manifests")
    check("replay: manifest in the decision's history", s == 200 and len(ms) >= 1, f"HTTP {s} {len(ms) if isinstance(ms, list) else ms}")
    s, _, all_ = gdl("GET", "/v1/replay-manifests?limit=5")
    check("replay: tenant-wide replay history carries the decision reference",
          s == 200 and any(r.get("decision_id") == decision_id and r.get("decision_display_ref") for r in all_.get("records", [])), f"HTTP {s}")



def replay_contract(state):
    d = state.get("parent")
    if not d:
        return
    s, _, b = gdl("POST", f"/v1/decisions/{d['decision_id']}/replay", body={})
    check("replay: non-threshold decision → 501 (no replay logic, stated, not faked)", s == 501, f"HTTP {s} {b.get('code')}")


# ── 6. identity (X5): verified envelope, peer workloads over mTLS ───────────

GDL_MTLS = ("127.0.0.1", 8450)
IDENTITY, MTLS = f"{H}:8080", f"{H}:8140"


def provision_cert(service):
    """A client certificate for `service` from mtls-management-svc, presenting
    that service's own bootstrap token (register M-01). Returns PEM file paths."""
    r = subprocess.run(["docker", "exec", "mtls-management-svc", "cat", f"/bootstrap-tokens/{service}/token"], capture_output=True)
    token = r.stdout.decode().strip()
    if not token:
        return None, f"no bootstrap token for {service}"
    h = {"X-Tenant-Id": PLATFORM, "X-Legal-Entity-Id": PLATFORM, "X-Workload-Id": service,
         "X-Request-Id": str(uuid.uuid4()), "X-Correlation-ID": str(uuid.uuid4()), "X-Source-Channel": "system",
         "X-Purpose-Context": "service_identity_provisioning", "Idempotency-Key": str(uuid.uuid4()),
         "X-Mtls-Bootstrap-Token": token}
    s, _, b = call("POST", f"{MTLS}/v1/mtls/certificates", h,
                   {"legal_entity_id": PLATFORM, "service_name": service, "common_name": service, "rotation_days": 1})
    if s != 201:
        return None, f"HTTP {s} {brief(b)}"
    d = os.path.join(tempfile.gettempdir(), f"gdl-e2e-{service}-{RUN}")
    os.makedirs(d, exist_ok=True)
    paths = {}
    for name, pem in (("cert", b["certificate"]["certificate_pem"]), ("key", b["private_key_pem"]), ("ca", b["ca_certificate_pem"])):
        paths[name] = os.path.join(d, name + ".pem")
        with open(paths[name], "w", encoding="ascii") as f:
            f.write(pem)
    return paths, ""


def mtls_call(paths, method, path, headers, body=None):
    ctx = ssl.create_default_context(cafile=paths["ca"])
    ctx.check_hostname = False  # the listener's certificate names the service, not 127.0.0.1
    if paths.get("cert"):
        ctx.load_cert_chain(paths["cert"], paths["key"])
    conn = http.client.HTTPSConnection(*GDL_MTLS, context=ctx, timeout=20)
    data = json.dumps(body).encode() if body is not None else None
    h = dict(headers)
    if data is not None:
        h["Content-Type"] = "application/json"
    conn.request(method, path, body=data, headers=h)
    r = conn.getresponse()
    raw = r.read()
    try:
        parsed = json.loads(raw) if raw else {}
    except ValueError:
        parsed = {}
    return r.status, dict(r.getheaders()), parsed


def envelope_jwt():
    s, _, b = call("POST", f"{IDENTITY}/v1/authenticate", {"Content-Type": "application/json"},
                   {"tenant_id": TENANT, "email": "admin@zoikosuite.com", "password": "Zoiko@Governance1",
                    "client_id": os.environ.get("IDENTITY_CONSOLE_CLIENT_ID", "zoiko-console"),
                    "client_secret": os.environ.get("IDENTITY_CONSOLE_CLIENT_SECRET", "local-dev-console-client-secret")})
    if s != 200 or not b.get("access_token"):
        return None, f"login HTTP {s} {brief(b)}"
    s, _, r = call("POST", f"{IDENTITY}/v1/context/resolve", envelope(write=True),
                   {"bearer_token": b["access_token"], "legal_entity_id": ENTITY})
    return (r.get("envelope_jwt"), "") if r.get("envelope_jwt") else (None, f"resolve HTTP {s} {brief(r)}")


def identity(require):
    mode = "require" if require else "warn"

    # Unverified: identity headers only. warn admits and marks it; require refuses.
    s, hd, b = call("GET", f"{GDL}/v1/decisions?limit=1", envelope())
    if require:
        check("identity (require): headers alone -> 401",
              s == 401, f"HTTP {s} {b.get('code')}")
    else:
        check("identity (warn): headers alone admitted but marked unverified",
              s == 200 and hd.get("X-Identity-Assertion") == "missing", f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")

    jwt, why = envelope_jwt()
    if check("identity: console envelope issued by identity-context-svc", jwt, why):
        h = envelope()
        h["Authorization"] = "Bearer " + jwt
        s, hd, b = call("GET", f"{GDL}/v1/decisions?limit=1", h)
        check(f"identity ({mode}): verified envelope -> 200, not marked unverified",
              s == 200 and hd.get("X-Identity-Assertion") != "missing", f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")
        head, payload, sig = jwt.split(".")
        h["Authorization"] = "Bearer " + ".".join([head, payload, sig[:-4] + ("AAAA" if not sig.endswith("AAAA") else "BBBB")])
        s, _, b = call("GET", f"{GDL}/v1/decisions?limit=1", h)
        check("identity: tampered envelope refused (401, every mode)", s == 401, f"HTTP {s} {b.get('code')}")

    peer, why = provision_cert("policy-svc")
    if not check("mtls: client certificate for policy-svc from mtls-management-svc", peer, why):
        return
    try:
        mtls_call({"ca": peer["ca"]}, "GET", "/healthz", {})
        check("mtls: :8450 refuses a client with no certificate", False, "handshake succeeded")
    except (ssl.SSLError, OSError, http.client.HTTPException) as e:
        check("mtls: :8450 refuses a client with no certificate", True, type(e).__name__)

    h = envelope("svc-policy", write=True)
    body = decision_body(action_type="APPROVAL_THRESHOLD", outcome="GRANTED", rule_basis="E2E-PEER:mtls",
                         title="Peer-relayed decision (mTLS)", tags=[])
    body.pop("priority")
    s, hd, d = mtls_call(peer, "POST", "/v1/decisions", h, body)
    check(f"mtls ({mode}): policy-svc as a registered peer records as svc-policy, verified",
          s == 201 and d.get("actor_id") == "svc-policy" and hd.get("X-Identity-Assertion") != "missing",
          f"HTTP {s} {d.get('actor_id')} assertion={hd.get('X-Identity-Assertion')} {brief(d) if s != 201 else ''}")

    stranger, why = provision_cert("tenant-entity-registry-svc")
    if check("mtls: client certificate for a non-peer workload", stranger, why):
        s, hd, b = mtls_call(stranger, "GET", "/v1/decisions?limit=1", envelope())
        if require:
            check("mtls (require): a verified certificate NOT in IDENTITY_PEERS cannot vouch -> 401", s == 401, f"HTTP {s} {b.get('code')}")
        else:
            check("mtls (warn): a certificate NOT in IDENTITY_PEERS is not a peer (marked unverified)",
                  hd.get("X-Identity-Assertion") == "missing", f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")


def ledger_mode():
    s, _, _ = call("GET", f"{GDL}/v1/decisions?limit=1", envelope())
    return "require" if s == 401 else "warn"


# ── 7. workflow-svc edge: a workflow decision relayed into the ledger (F10) ──

WORKFLOW = f"{H}:8090"


def workflow():
    s, _, _ = call("GET", f"{WORKFLOW}/healthz", timeout=5)
    if not check("health workflow-svc /healthz", s == 200, f"HTTP {s}"):
        return
    subject = f"PR-{RUN}"
    h = envelope(ADMIN, write=True)
    s, _, wf = call("POST", f"{WORKFLOW}/v1/workflows", h, {
        "tenant_id": TENANT, "legal_entity_id": ENTITY, "workflow_type": "PURCHASE_APPROVAL",
        "subject_type": "PURCHASE_REQUEST", "subject_id": subject,
        "stages": [{"stage_type": "SINGLE", "approver_principal_id": APPROVER}]})
    if not check("workflow: instance created by the demo admin", s in (200, 201) and wf.get("workflow_instance_id"),
                 f"HTTP {s} {brief(wf)}"):
        return
    wid = wf["workflow_instance_id"]
    # Optimistic concurrency (API-3): the instance's current ETag in If-Match.
    _, wh, _ = call("GET", f"{WORKFLOW}/v1/workflows/{wid}", envelope(APPROVER))
    ah = envelope(APPROVER, write=True)
    if wh and wh.get("ETag"):
        ah["If-Match"] = wh.get("ETag")
    s, _, act = call("POST", f"{WORKFLOW}/v1/workflows/{wid}/actions", ah,
                     {"action": "APPROVE", "rationale": "e2e: within budget"})
    if not check("workflow: the approver (a different person) approves", s in (200, 201), f"HTTP {s} {brief(act)}"):
        return

    def relayed():
        s, _, lst = gdl("GET", f"/v1/decisions?workflow={wid}")
        recs = lst.get("records", []) if s == 200 else []
        return (bool(recs), recs)

    ok, recs = until(relayed, 60, 2)
    check("workflow relay: the decision is in the ledger, written by svc-workflow over mTLS",
          ok and recs[0].get("actor_id") == "svc-workflow", brief(recs[:1]))
    if ok:
        d = recs[0]
        check("workflow relay: workflow instance, subject and rule basis recorded",
              d.get("workflow_instance_id") == wid and d.get("action_subject_id") == subject and d.get("rule_basis", "").startswith("workflow:"),
              f"{d.get('workflow_instance_id')} {d.get('action_subject_id')} {d.get('rule_basis')}")
        s, _, ev = gdl("GET", f"/v1/decisions/{d['decision_id']}/evidence")
        check("workflow relay: the relayed decision is linked into the evidence chain",
              s == 200 and ev.get("hash_valid") and ev.get("prev_link_valid"), brief(ev)[:200])


# ── 8. the decision log's readers (F07) at the ledger ───────────────────────
#
# Each reader's own certificate (its own bootstrap token), the exact headers its
# client sends, and the principal it vouches for.

READER_SERVICES = {"contract-lifecycle-svc": ("svc-contract-lifecycle", "contract_activation_verification"),
                   "decision-support-svc": ("svc-decision-support", "decision_support_precedent")}


def reader_headers(principal, purpose):
    rid = uuid.uuid4().hex
    return {"X-Tenant-Id": TENANT, "X-Principal-Id": principal, "X-Request-Id": rid,
            "X-Correlation-ID": f"e2e-reader-{RUN}-{rid[:8]}", "X-Source-Channel": "system",
            "X-Purpose-Context": purpose, "X-Legal-Entity-Id": ENTITY}


def readers_at_ledger(state):
    d = state.get("parent")
    if not d:
        skip("F07 readers", "no decision recorded")
        return
    for svc, (principal, purpose) in READER_SERVICES.items():
        paths, why = provision_cert(svc)
        if not check(f"F07: {svc} certificate from mtls-management-svc with its own bootstrap token", paths, why):
            continue
        s, hd, b = mtls_call(paths, "GET", f"/v1/decisions/{d['decision_id']}", reader_headers(principal, purpose))
        check(f"F07: {svc} reads one decision as {principal} over mTLS (peer, verified, full envelope) → 200",
              s == 200 and b.get("decision_id") == d["decision_id"] and hd.get("X-Identity-Assertion") != "missing"
              and "violated" not in (hd.get("X-Envelope-Contract") or ""),
              f"HTTP {s} assertion={hd.get('X-Identity-Assertion')} contract={hd.get('X-Envelope-Contract')} {brief(b) if s != 200 else ''}")
        s, _, lst = mtls_call(paths, "GET", f"/v1/decisions?entity={ENTITY}&action=VENDOR_CONTRACT_RENEWAL&limit=5",
                              reader_headers(principal, purpose))
        check(f"F07: {svc} lists the entity's decisions as a page (records[])",
              s == 200 and isinstance(lst.get("records"), list) and len(lst["records"]) >= 1, f"HTTP {s} total {lst.get('total_count')}")
        s, _, b = mtls_call(paths, "POST", "/v1/decisions", envelope(principal, write=True), decision_body())
        check(f"F07: {principal} is read-only: a write → 403", s == 403, f"HTTP {s} {b.get('code')}")
        h = reader_headers(principal, purpose)
        s, _, b = call("GET", f"{GDL}/v1/decisions/{d['decision_id']}", h)
        check(f"F07: the same read with bare headers on :8083 (the old client) → 401", s == 401, f"HTTP {s} {b.get('code')}")

    em, why = provision_cert("evidence-manifest-svc")
    if check("F07: evidence-manifest-svc certificate with its own bootstrap token", em, why):
        s, hd, lst = mtls_call(em, "GET", f"/v1/decisions?entity={ENTITY}&limit=5", reader_headers(ADMIN, "evidence_manifest_assembly"))
        check("F07: evidence-manifest-svc vouching for the signed-in user (who holds READ) → 200",
              s == 200 and isinstance(lst.get("records"), list), f"HTTP {s} assertion={hd.get('X-Identity-Assertion')}")
        s, _, b = mtls_call(em, "GET", f"/v1/decisions?entity={ENTITY}&limit=5", reader_headers(STRANGER, "evidence_manifest_assembly"))
        check("F07: evidence-manifest-svc vouching for a user WITHOUT READ → 403 (the person is authorized, not the service)",
              s == 403, f"HTTP {s} {b.get('code')}")


# ── 9. the readers themselves, live (F07) ───────────────────────────────────

CONTRACTS, RECOMMENDATIONS, MANIFESTS = f"{H}:8119", f"{H}:8138", f"{H}:8095"
E2E_READER_ROLE = "e2e00000-0000-4000-8000-00000000f070"


def grant_demo(actions):
    """DEVELOPMENT ONLY: the reader services' own actions for the two demo
    principals (the seed has no bundles for these teammates' services)."""
    acts = json.dumps(actions)
    sql = f"""
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('{E2E_READER_ROLE}', '{TENANT}', 'E2E_DECISION_LOG_READERS', 'e2e: decision-log reader services', 'LEGAL_ENTITY', true, 'e2e')
ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('{E2E_READER_ROLE}', 'E2E_READER_SERVICES', '{acts}'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT p.principal, '{E2E_READER_ROLE}', '{ENTITY}'::uuid, now() - interval '1 minute', 'e2e'
FROM (VALUES ('{ADMIN}'), ('{APPROVER}')) AS p(principal)
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments a WHERE a.principal_id = p.principal AND a.role_id = '{E2E_READER_ROLE}');
"""
    rc, out, err = psql(sql, db="authorization_svc")
    return rc == 0, err


def record_granted(action, outcome):
    s, _, d = gdl("POST", "/v1/decisions", body=decision_body(action_type=action, outcome=outcome,
                                                              rule_basis="E2E-F07:" + action, title=f"F07 {action} {outcome}"))
    return d.get("decision_id") if s == 201 else None


def readers_live():
    ok, why = grant_demo(["CONTRACT_CREATE", "CONTRACT_SUBMIT_REVIEW", "CONTRACT_APPROVE",
                          "DECISION_SUPPORT_REQUEST", "DECISION_SUPPORT_VIEW",
                          "EVIDENCE_MANIFEST_GENERATE", "EVIDENCE_MANIFEST_READ"])
    check("F07 live: demo grants for the reader services' own actions", ok, why)
    time.sleep(6)  # authorization-svc's decision cache

    granted = [record_granted("CONTRACT_APPROVE", "GRANTED") for _ in range(2)]
    denied = record_granted("CONTRACT_APPROVE", "DENIED")
    check("F07 live: CONTRACT_APPROVE decisions recorded for the entity (2 GRANTED, 1 DENIED)", all(granted) and denied,
          f"{granted} {denied}")

    # contract-lifecycle-svc: activation verifies its governance decision.
    s, _, _ = call("GET", f"{CONTRACTS}/healthz", timeout=5)
    if check("health contract-lifecycle-svc /healthz", s == 200, f"HTTP {s}"):
        s, _, c = call("POST", f"{CONTRACTS}/v1/contracts", envelope(ADMIN, write=True), {
            "legal_entity_id": ENTITY, "contract_type": "VENDOR", "title": f"F07 contract {RUN}",
            "counterparty_id": "cp-" + RUN, "counterparty_name": "E2E Vendor Ltd", "effective_from": "2026-11-11",
            "currency": "GBP", "total_value": 1000, "created_by": ADMIN})
        cid = c.get("contract_id") or c.get("id")
        if check("contract-lifecycle: contract created", s in (200, 201) and cid, f"HTTP {s} {brief(c)}"):
            s, _, b = call("POST", f"{CONTRACTS}/v1/contracts/{cid}/submit-review", envelope(ADMIN, write=True), {})
            check("contract-lifecycle: submitted for review", s == 200, f"HTTP {s} {brief(b)}")
            s, _, b = call("POST", f"{CONTRACTS}/v1/contracts/{cid}/approve", envelope(APPROVER, write=True),
                           {"governance_decision_id": denied})
            check("contract-lifecycle → decision log: a DENIED decision refuses approval (403, read over mTLS)",
                  s == 403, f"HTTP {s} {brief(b)}")
            s, _, b = call("POST", f"{CONTRACTS}/v1/contracts/{cid}/approve", envelope(APPROVER, write=True),
                           {"governance_decision_id": "no-such-" + RUN})
            check("contract-lifecycle → decision log: an unknown decision refuses approval (400)", s == 400, f"HTTP {s} {brief(b)}")
            s, _, b = call("POST", f"{CONTRACTS}/v1/contracts/{cid}/approve", envelope(APPROVER, write=True),
                           {"governance_decision_id": granted[0]})
            check("contract-lifecycle → decision log: the GRANTED decision approves the contract (read as svc-contract-lifecycle over mTLS)",
                  s == 200 and b.get("status") == "APPROVED", f"HTTP {s} {brief(b)}")

    # decision-support-svc: precedent from the ledger, not NO_HISTORY.
    s, _, _ = call("GET", f"{RECOMMENDATIONS}/healthz", timeout=5)
    if check("health decision-support-svc /healthz", s == 200, f"HTTP {s}"):
        h = envelope(ADMIN, write=True)
        s, _, rec = call("POST", f"{RECOMMENDATIONS}/v1/recommendations", h, {
            "legal_entity_id": ENTITY, "subject_type": "CONTRACT", "subject_reference": "F07-" + RUN,
            "action_type": "CONTRACT_APPROVE", "correlation_id": h["X-Correlation-ID"]})
        check("decision-support → decision log: recommendation built from ledger precedent (records[] decoded, over mTLS)",
              s == 201 and rec.get("recommended_action") == "APPROVE" and rec.get("prior_decisions_sampled", 0) >= 3
              and "GRANTED in" in rec.get("rationale", ""),
              f"HTTP {s} {rec.get('recommended_action')} sampled {rec.get('prior_decisions_sampled')} {rec.get('rationale', '')[:80]}")

    # evidence-manifest-svc: a range read for the signed-in user, every page.
    s, _, _ = call("GET", f"{MANIFESTS}/healthz", timeout=5)
    if check("health evidence-manifest-svc /healthz", s == 200, f"HTTP {s}"):
        now = datetime.now(timezone.utc)
        body = {"tenant_id": TENANT, "legal_entity_id": ENTITY, "scenario_type": "AUDIT",
                "governance_decisions_from": (now - timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ"),
                "governance_decisions_to": (now + timedelta(minutes=5)).strftime("%Y-%m-%dT%H:%M:%SZ")}
        mh = envelope(ADMIN, write=True)
        mh["X-Purpose-Context"] = "AUDIT_EVIDENCE"
        s, _, m = call("POST", f"{MANIFESTS}/v1/evidence-manifests", mh, body)
        mid = m.get("manifest_id")
        check("evidence-manifest: manifest GENERATED", s == 201 and m.get("status") == "GENERATED", f"HTTP {s} {brief(m)[:200]}")
        recs = []
        if mid:
            rh = envelope(ADMIN)
            rh["X-Purpose-Context"] = "AUDIT_EVIDENCE"
            _, _, recs = call("GET", f"{MANIFESTS}/v1/evidence-manifests/{mid}/records", rh)
            recs = recs if isinstance(recs, list) else []
        ids = {r.get("source_record_id") for r in recs if isinstance(r, dict)}
        check("evidence-manifest → decision log: manifest snapshots the entity's decisions (pages followed, user vouched over mTLS)",
              all(g in ids for g in granted) and denied in ids
              and all(r.get("source_type") == "GOVERNANCE_DECISION" for r in recs if r.get("source_record_id") in ids),
              f"{len(recs)} records in the manifest")
        sh = envelope(STRANGER, write=True)
        sh["X-Purpose-Context"] = "AUDIT_EVIDENCE"
        s, _, m = call("POST", f"{MANIFESTS}/v1/evidence-manifests", sh, body)
        check("evidence-manifest: a user without the manifest grant is refused (403)", s == 403, f"HTTP {s}")


# ── 10. production hardening on :8450: revocation (CRL) and rotation ────────

# Platform certificates belong to the platform tenant, so the revoker's role is
# a platform-tenant role.
MTLS_ADMIN_ROLE = "e2e00000-0000-4000-8000-00000000f072"


def hardening():
    s, hd, b = call("GET", f"{MTLS}/v1/mtls/crl", raw=True)
    check("CRL: mtls-management-svc publishes its CA-signed CRL", s == 200 and len(b) > 0, f"HTTP {s} {len(b)} bytes")
    logs = subprocess.run(["docker", "logs", "governance-decision-log-svc"], capture_output=True)
    text = (logs.stdout + logs.stderr).decode("utf8", "replace")
    check("rotation: workload identity installed with a renewal point (2/3 of its lifetime)",
          "workload identity provisioned" in text and "renew_at" in text, "renew_at logged" if "renew_at" in text else "not logged")
    check("revocation: :8450 runs the CRL check", "client certificate revocation check on" in text, "")

    # A fresh decision-support-svc certificate works, is revoked, and is then
    # refused at the handshake while another peer's keeps working.
    s, _, issued = issue_cert("decision-support-svc")
    if not check("revocation: a peer certificate issued (decision-support-svc)", s == 201, f"HTTP {s} {brief(issued)[:120]}"):
        return
    paths = write_pem("dsr", issued)
    probe = reader_headers("svc-decision-support", "decision_support_precedent")
    s, _, _ = mtls_call(paths, "GET", "/v1/decisions?limit=1", probe)
    check("revocation: before revoking, the peer is admitted (200)", s == 200, f"HTTP {s}")

    acts = json.dumps(["MTLS_CERT_REVOKE"])
    rc, _, err = psql(f"""
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('{MTLS_ADMIN_ROLE}', '{PLATFORM}', 'E2E_MTLS_REVOKER', 'e2e: certificate revoker', 'LEGAL_ENTITY', true, 'e2e') ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('{MTLS_ADMIN_ROLE}', 'E2E_MTLS_REVOKE', '{acts}'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '{ADMIN}', '{MTLS_ADMIN_ROLE}', '{PLATFORM}'::uuid, now() - interval '1 minute', 'e2e'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments WHERE principal_id = '{ADMIN}' AND role_id = '{MTLS_ADMIN_ROLE}');
""", db="authorization_svc")
    check("revocation: demo admin granted MTLS_CERT_REVOKE (dev only)", rc == 0, err)
    time.sleep(6)
    cert_id = issued["certificate"].get("certificate_id") or issued["certificate"].get("id")
    h = envelope(ADMIN, write=True, tenant=PLATFORM, entity=PLATFORM)
    h["X-Purpose-Context"] = "service_identity_revocation"
    h["Authorization"] = "Bearer " + admin_jwt()
    s, _, b = call("DELETE", f"{MTLS}/v1/mtls/certificates/{cert_id}", h)
    if s in (401, 403):
        h.pop("Authorization")
        s, _, b = call("DELETE", f"{MTLS}/v1/mtls/certificates/{cert_id}", h)
    if not check("revocation: certificate revoked in mtls-management-svc", s == 200, f"HTTP {s} {brief(b)}"):
        return

    def refused():
        try:
            st, _, _ = mtls_call(paths, "GET", "/v1/decisions?limit=1", probe)
            return (False, f"HTTP {st}")
        except (ssl.SSLError, OSError, http.client.HTTPException) as e:
            return (True, type(e).__name__)

    ok, how = until(refused, 75, 3)
    check("revocation: the revoked certificate is refused at the :8450 handshake within the CRL refresh", ok, how)
    other, why = provision_cert("contract-lifecycle-svc")
    if other:
        s, _, _ = mtls_call(other, "GET", "/v1/decisions?limit=1", reader_headers("svc-contract-lifecycle", "contract_activation_verification"))
        check("revocation: another peer's certificate is still admitted", s == 200, f"HTTP {s}")


def issue_cert(service):
    r = subprocess.run(["docker", "exec", "mtls-management-svc", "cat", f"/bootstrap-tokens/{service}/token"], capture_output=True)
    token = r.stdout.decode().strip()
    h = {"X-Tenant-Id": PLATFORM, "X-Legal-Entity-Id": PLATFORM, "X-Workload-Id": service,
         "X-Request-Id": str(uuid.uuid4()), "X-Correlation-ID": str(uuid.uuid4()), "X-Source-Channel": "system",
         "X-Purpose-Context": "service_identity_provisioning", "Idempotency-Key": str(uuid.uuid4()),
         "X-Mtls-Bootstrap-Token": token}
    return call("POST", f"{MTLS}/v1/mtls/certificates", h,
                {"legal_entity_id": PLATFORM, "service_name": service, "common_name": service, "rotation_days": 1})


def write_pem(tag, b):
    d = os.path.join(tempfile.gettempdir(), f"gdl-e2e-{tag}-{RUN}")
    os.makedirs(d, exist_ok=True)
    paths = {}
    for name, pem in (("cert", b["certificate"]["certificate_pem"]), ("key", b["private_key_pem"]), ("ca", b["ca_certificate_pem"])):
        paths[name] = os.path.join(d, name + ".pem")
        with open(paths[name], "w", encoding="ascii") as f:
            f.write(pem)
    return paths


# ── 12. event contracts in schema-registry-svc (R9: wire + canonical names) ─

SCHEMA_REGISTRY = f"{H}:8093"
SCHEMA_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "services",
                          "governance-decision-log-svc", "schemas")
SCHEMA_NAMES = [("governance.decision.recorded", "governance.decision.recorded.v1.json"),
                ("com.zoikosuite.governance.decision.recorded", "governance.decision.recorded.v1.json"),
                ("governance.decision.replayed", "governance.decision.replayed.v1.json"),
                ("com.zoikosuite.governance.decision.replayed", "governance.decision.replayed.v1.json")]


def schema_contracts(state):
    s, _, _ = call("GET", f"{SCHEMA_REGISTRY}/healthz", timeout=5)
    if not check("health schema-registry-svc /healthz", s == 200, f"HTTP {s}"):
        return
    registered = {}
    for name, file in SCHEMA_NAMES:
        with open(os.path.join(SCHEMA_DIR, file), encoding="utf-8") as f:
            want = json.load(f)
        s, _, b = call("GET", f"{SCHEMA_REGISTRY}/v1/schemas/{name}/versions/latest", envelope(ADMIN))
        got = b.get("json_schema")
        check(f"R9: {name} registered, owned by governance-decision-log-svc, contract = the service's schema file",
              s == 200 and got == want and b.get("owning_service") == "governance-decision-log-svc",
              f"HTTP {s} v{b.get('version')} owner {b.get('owning_service')} {'same' if got == want else 'DIFFERS'}")
        registered[name] = got or {}

    # A live event, as consumed from the broker, carries every field its
    # registered contract requires, under a name the contract lists.
    d = state.get("parent")
    if not d:
        return
    r = subprocess.run(["docker", "exec", KAFKA, "/opt/kafka/bin/kafka-console-consumer.sh", "--bootstrap-server", "localhost:9092",
                        "--topic", TOPIC, "--from-beginning", "--timeout-ms", "15000"], capture_output=True)
    events = [json.loads(line) for line in r.stdout.decode("utf8", "replace").splitlines()
              if d["decision_id"] in line and line.startswith("{")]
    events = [e for e in events if e.get("aggregate_id") == d["decision_id"]]
    for name in ("governance.decision.recorded", "com.zoikosuite.governance.decision.recorded"):
        e = next((x for x in events if x.get("event_type") == name), None)
        contract = registered.get(name, {})
        missing = [k for k in contract.get("required", []) if k not in (e or {})]
        pmissing = [k for k in contract.get("properties", {}).get("payload", {}).get("required", [])
                    if k not in ((e or {}).get("payload") or {})]
        check(f"R9: the live {name} event satisfies its registered contract (required envelope + payload fields, name in enum)",
              e is not None and not missing and not pmissing
              and name in contract.get("properties", {}).get("event_type", {}).get("enum", []),
              f"missing {missing + pmissing}" if e else "event not found on the topic")


# ── 11. scheduled sealing (run with DECISION_LOG_EVIDENCE_SEAL_INTERVAL short) ─

def scheduled_seal(wait):
    s, _, d = gdl("POST", "/v1/decisions", body=decision_body(title="Scheduled-seal probe", tags=[]))
    if not check("scheduled seal: a new decision recorded", s == 201, f"HTTP {s}"):
        return

    def sealed():
        st, _, pk = gdl("GET", "/v1/evidence/packages")
        pkgs = pk.get("records", []) if st == 200 else []
        mine = [p for p in pkgs if p.get("sealed_by") == "svc-governance-decision-log" and p.get("to_seq", 0) >= d["chain_seq"]]
        return (bool(mine), mine)

    ok, mine = until(sealed, wait, 3)
    check("scheduled seal: the sealer sealed through the new decision as svc-governance-decision-log",
          ok, brief(mine[:1]) if ok else "no scheduled package covering it")
    if ok:
        s, _, v = gdl("GET", f"/v1/evidence/packages/{mine[0]['package_id']}/verify")
        check("scheduled seal: its Merkle root and package hash re-compute", s == 200 and v.get("valid") is True, brief(v)[:160])
        s, _, v = gdl("GET", "/v1/evidence/chain/verify")
        check("scheduled seal: whole chain still verifies", s == 200 and v.get("valid") is True, brief(v)[:120])


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--no-policy", action="store_true", help="skip the policy-svc edge (reported as SKIP)")
    ap.add_argument("--no-workflow", action="store_true", help="skip the workflow-svc edge (reported as SKIP)")
    ap.add_argument("--require", action="store_true",
                    help="the service runs IDENTITY_VERIFY=require: run only the identity checks, expecting refusals")
    ap.add_argument("--no-readers", action="store_true",
                    help="skip the live reader services (contract-lifecycle, decision-support, evidence-manifest); reported as SKIP")
    ap.add_argument("--scheduled-seal", type=int, default=0, metavar="SECONDS",
                    help="run only the scheduled-sealing check, waiting up to SECONDS (start the service with a short "
                         "DECISION_LOG_EVIDENCE_SEAL_INTERVAL)")
    a = ap.parse_args()
    if a.scheduled_seal:
        scheduled_seal(a.scheduled_seal)
        failed = [r for r in results if r[1] == "FAIL"]
        print(f"\n{len(results) - len(failed)} passed, {len(failed)} failed")
        return len(failed)

    mode = ledger_mode()
    print(f"governance-decision-log-svc identity mode: {mode}")
    if a.require:
        identity(True)
        failed = [r for r in results if r[1] == "FAIL"]
        print(f"\n{len(results) - len(failed)} passed, {len(failed)} failed")
        return len(failed)

    health()
    schema()
    authz_grants()
    state = ledger()
    kafka(state)
    replay_contract(state)
    evidence()
    if a.no_policy:
        skip("policy-svc edge", "--no-policy")
    else:
        policy()
    if a.no_workflow:
        skip("workflow-svc edge", "--no-workflow")
    else:
        workflow()
    identity(mode == "require")
    readers_at_ledger(state)
    if a.no_readers:
        skip("F07 live reader services", "--no-readers")
    else:
        readers_live()
    hardening()
    schema_contracts(state)

    s, _, metrics = call("GET", f"{GDL}/metrics", raw=True)
    text = metrics.decode("utf8", "replace") if isinstance(metrics, bytes) else ""
    check("metrics: outbox gauges exported", s == 200 and "outbox_pending_events" in text, f"HTTP {s}")

    failed = [r for r in results if r[1] == "FAIL"]
    skipped = [r for r in results if r[1] == "SKIP"]
    print(f"\n{len(results) - len(failed) - len(skipped)} passed, {len(failed)} failed, {len(skipped)} skipped")
    for name, _, detail in failed:
        print(f"  FAIL {name}: {detail}")
    return len(failed)


if __name__ == "__main__":
    sys.exit(main())
