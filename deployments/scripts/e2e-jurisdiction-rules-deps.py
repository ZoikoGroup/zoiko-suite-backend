#!/usr/bin/env python3
"""End-to-end check of jurisdiction-rules-svc and every service it depends on.

Each check crosses one real dependency edge on the local compose stack, so a
failure names the edge that is broken:

  postgres                  41 migrations; RLS forced on the five tenant tables;
                            the runtime role is not a superuser and cannot bypass
                            RLS (a tenant-less read of tenant data sees nothing)
  identity-context-svc      the demo admin's and the approver's signed envelopes;
                            J-01: an administrative write with identity headers
                            only, or with a forged basis header, is 401
  authorization-svc         a principal without grants is refused (403); the
                            granted admin is served; access-control 000052 templates
  kafka                     a registry event reaches zoiko.jurisdiction.events from
                            the outbox, with its aggregate_version (J-11, J-12)
  workflow-svc              J-02 live: an activation citing an unknown, a PENDING, a
                            REJECTED, or another rule's APPROVED workflow is refused
                            (403 approval_not_valid) and changes nothing; one citing
                            an APPROVED workflow bound to the rule and decided by
                            the approver activates it, with the reference on its
                            status history. Without workflow-svc: the fail-closed 503
  regulatory feed (kafka)   JR-6 live, when the service runs with
                            KAFKA_REGULATORY_FEED_TOPIC: a regulatory.source.changed
                            event (published here; its real producer,
                            external-data-feed-svc, does not publish it yet) moves
                            the ACTIVE rule citing the reference to UNDER_REVIEW,
                            records the drift and legal.drift.detected; the same
                            event redelivered changes nothing
  tenant-entity-registry    an entity created there (primary jurisdiction checked
                            against this service) is projected by the consumer;
                            after its mapping is lost, the J-10 backfill restores it,
                            and a rerun changes nothing
  the service itself        pack lifecycle with maker-checker across two verified
                            principals, async compile (API-5), If-Match 412 (API-3),
                            sign and verify with the dev signing key, cursor paging
                            (API-4), parked-event requeue (J-16), rule effectivity
                            event (the rule activated through workflow-svc), console
                            summary

    python deployments/scripts/e2e-jurisdiction-rules-deps.py [--prepare-signing-key] [--no-workflow]

--prepare-signing-key writes a throwaway Ed25519 key to deployments/jurisdiction/
dev-keys (gitignored) and prints the command that restarts the service with it
(and with the feed consumer on); signing is checked only when the service runs
with that key. --no-workflow runs without workflow-svc: the J-02 refusal is then
the fail-closed 503, the effectivity rule is activated by a database fixture, and
the workflow and feed sections are reported as SKIP. First run
identity-context-svc's seed-demo-principals (the approver logs in as
legal.counsel@) and seed-demo-rbac.ps1 (the demo admin and the approver need
JURISDICTION_FULL and JURISDICTION_PACK_REGISTRY_FULL). The entity edge adds a
development-only ENTITY_CREATE grant for the demo admin. What this writes
stays: the registry is append-only by design.

Exit status is the number of failed checks.
"""
import argparse, base64, json, os, subprocess, sys, time, urllib.error, urllib.parse, urllib.request, uuid
from datetime import datetime, timedelta, timezone

sys.stdout.reconfigure(encoding="utf-8")

TENANT = "11111111-1111-1111-1111-111111111111"
OTHER_TENANT = "99999999-8888-4777-8666-555555555555"
ENTITY = "22222222-2222-2222-2222-222222222222"
ADMIN = "33333333-3333-3333-3333-333333333333"
APPROVER = "66666666-6666-6666-6666-666666666666"
STRANGER = "e2e00000-0000-4000-8000-0000000000bb"
RESIDENCY_POLICY = "77777777-7777-7777-7777-777777777777"
FISCAL_CALENDAR = "99999999-9999-9999-9999-999999999999"

H = "http://127.0.0.1"
JR, IDENTITY, TER, WF = f"{H}:8082", f"{H}:8080", f"{H}:8081", f"{H}:8090"
PG, KAFKA, CONTAINER = "zoiko-postgres", "zoiko-kafka", "jurisdiction-rules-svc"
TOPIC = "zoiko.jurisdiction.events"
RUN = uuid.uuid4().hex[:8]
WORKFLOW_LIVE = True  # main() clears it with --no-workflow or when workflow-svc is down
KEY_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "jurisdiction", "dev-keys")
KEY_REF = "jur-dev-signing"

results = []


def check(name, ok, detail=""):
    results.append((name, "PASS" if ok else "FAIL", detail))
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  — {detail}" if detail else ""), flush=True)
    return bool(ok)


def skip(name, why):
    results.append((name, "SKIP", why))
    print(f"SKIP  {name}  — {why}", flush=True)


def brief(body):
    return json.dumps(body)[:240]


def call(method, url, headers=None, body=None, timeout=25):
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
    try:
        parsed = json.loads(content) if content else {}
    except ValueError:
        parsed = {"raw": content[:200].decode("utf8", "replace")}
    return status, hdrs, parsed


def psql(sql, db="jurisdiction_rules"):
    r = subprocess.run(["docker", "exec", "-i", PG, "psql", "-X", "-q", "-U", "postgres", "-d", db, "-tA", "-c", sql], capture_output=True)
    return r.stdout.decode("utf8", "replace").strip()


def container_env():
    out = subprocess.run(["docker", "inspect", CONTAINER, "--format", "{{range .Config.Env}}{{println .}}{{end}}"], capture_output=True).stdout.decode()
    return dict(l.split("=", 1) for l in out.splitlines() if "=" in l)


def until(fn, seconds, step=1.0):
    deadline = time.time() + seconds
    while True:
        v = fn()
        if v or time.time() > deadline:
            return v
        time.sleep(step)


def base_headers(principal, tenant=TENANT, write=False):
    rid = str(uuid.uuid4())
    h = {"X-Tenant-Id": tenant, "X-Principal-Id": principal, "X-Legal-Entity-Id": ENTITY,
         "X-Correlation-ID": f"e2e-jur-{RUN}-{rid[:8]}", "X-Request-Id": rid, "X-Source-Channel": "api",
         "X-Source-System": "e2e-jurisdiction-rules-deps"}
    if write:
        h["Idempotency-Key"] = str(uuid.uuid4())
    return h


ACCOUNTS = {ADMIN: ("admin@zoikosuite.com", "Zoiko@Governance1"), APPROVER: ("legal.counsel@zoikosuite.com", "Zoiko@Legal2026!")}
_ENVELOPES = {}


def envelope_jwt(principal):
    cached = _ENVELOPES.get(principal)
    if cached and time.time() - cached[1] < 200:
        return cached[0], ""
    email, password = ACCOUNTS[principal]
    s, _, b = call("POST", f"{IDENTITY}/v1/authenticate", {"Content-Type": "application/json"},
                   {"tenant_id": TENANT, "email": email, "password": password,
                    "client_id": os.environ.get("IDENTITY_CONSOLE_CLIENT_ID", "zoiko-console"),
                    "client_secret": os.environ.get("IDENTITY_CONSOLE_CLIENT_SECRET", "local-dev-console-client-secret")})
    if s != 200 or not b.get("access_token"):
        return None, f"login {email}: HTTP {s} {brief(b)}"
    s, _, r = call("POST", f"{IDENTITY}/v1/context/resolve", base_headers(principal, write=True),
                   {"bearer_token": b["access_token"], "legal_entity_id": ENTITY})
    if not r.get("envelope_jwt"):
        return None, f"resolve {email}: HTTP {s} {brief(r)}"
    _ENVELOPES[principal] = (r["envelope_jwt"], time.time())
    return r["envelope_jwt"], ""


def jr(method, path, principal=ADMIN, body=None, headers=None, verified=True, tenant=TENANT, base=JR):
    h = base_headers(principal, tenant=tenant, write=method != "GET")
    if verified:
        jwt, _ = envelope_jwt(principal)
        if jwt:
            h["Authorization"] = "Bearer " + jwt
    h.update(headers or {})
    return call(method, base + path, h, body)


def iso(dt):
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# ── sections ────────────────────────────────────────────────────────────────

def preflight():
    s, _, _ = call("GET", f"{JR}/healthz")
    check("service: /healthz", s == 200, f"HTTP {s}")
    s, _, b = call("GET", f"{JR}/readyz")
    check("service: /readyz (database pool ok)", s == 200, f"HTTP {s} {brief(b)}")
    files = [f for f in os.listdir(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "services",
                                                "jurisdiction-rules-svc", "deployments", "migrations")) if f.endswith(".up.sql")]
    applied = psql("select count(*) from schema_migrations")
    check("postgres: every migration applied", applied == str(len(files)), f"{applied}/{len(files)}")
    check("postgres: RLS enabled and forced on the five tenant tables",
          psql("select count(*) from pg_class where relname in ('entity_jurisdiction_applicability','regulatory_submissions',"
               "'regulatory_submission_events','rule_decision_evidence','async_operations') and relrowsecurity and relforcerowsecurity") == "5")
    check("postgres: runtime role is not a superuser and cannot bypass RLS",
          psql("select count(*) from pg_roles where rolname='app_jurisdiction_rules' and not rolsuper and not rolbypassrls") == "1")
    port = subprocess.run(["docker", "port", CONTAINER, "8082"], capture_output=True).stdout.decode().strip()
    check("J-01: the service port is published on the loopback interface only", port and all(p.startswith("127.0.0.1:") for p in port.splitlines()), port)


def identity_and_authz():
    for p in (ADMIN, APPROVER):
        jwt, why = envelope_jwt(p)
        check(f"identity: signed envelope for {ACCOUNTS[p][0]}", jwt, why)
    body = {"jurisdiction_code": f"E2E-H{RUN}".upper(), "jurisdiction_name": "headers only", "jurisdiction_type": "COUNTRY",
            "authority_type": "NATIONAL", "effective_from": "2026-01-01T00:00:00Z"}
    s, _, b = jr("POST", "/v1/admin/jurisdictions", body=body, verified=False)
    check("J-01: admin write with identity headers only -> 401 identity_not_verified", s == 401 and b.get("code") == "identity_not_verified", f"HTTP {s} {b.get('code')}")
    s, _, b = jr("POST", "/v1/admin/jurisdictions", body=body, verified=False, headers={"X-Identity-Basis": "verified_envelope"})
    check("J-01: a forged X-Identity-Basis header is not a verification -> 401", s == 401, f"HTTP {s} {b.get('code')}")
    s, h, _ = jr("GET", "/v1/jurisdictions?page_size=1", verified=False)
    check("identity (warn for reads): headers-only read admitted and marked unverified", s == 200 and h.get("X-Identity-Assertion") == "missing",
          f"HTTP {s} assertion={h.get('X-Identity-Assertion')}")
    s, _, b = jr("GET", "/v1/jurisdiction-summary", principal=STRANGER, verified=False)
    check("authorization: a principal without grants is refused (403)", s == 403, f"HTTP {s} {b.get('code')}")
    s, _, b = jr("GET", "/v1/jurisdiction-summary")
    check("authorization: the granted admin reads the console summary", s == 200 and "kpis" in b and "pack_pipeline" in b, f"HTTP {s} {brief(b)}")
    check("access-control 000052: nine jurisdiction role templates published",
          psql("select count(*) from role_templates where template_code like 'JURISDICTION_%' and template_code <> 'JURISDICTION_COMPLIANCE_OFFICER'",
               db="access_control") == "9")


def registry_and_kafka():
    code = f"E2E{RUN}".upper()
    body = {"jurisdiction_code": code, "jurisdiction_name": f"E2E land {RUN}", "jurisdiction_type": "COUNTRY",
            "authority_type": "NATIONAL", "effective_from": "2025-01-01T00:00:00Z"}
    key = {"Idempotency-Key": f"e2e-jur-create-{RUN}"}
    s, h, j = jr("POST", "/v1/admin/jurisdictions", body=body, headers=key)
    if not check("registry: verified admin creates a jurisdiction (201)", s == 201 and j.get("jurisdiction_id"), f"HTTP {s} {brief(j)}"):
        return None
    s2, h2, j2 = jr("POST", "/v1/admin/jurisdictions", body=body, headers=key)
    check("registry: the same Idempotency-Key replays the original answer (201, same id, Idempotent-Replayed)",
          s2 == 201 and j2.get("jurisdiction_id") == j["jurisdiction_id"] and h2.get("Idempotent-Replayed") == "true",
          f"HTTP {s2} replayed={h2.get('Idempotent-Replayed') if hasattr(h2, 'get') else None}")
    jid = j["jurisdiction_id"]

    published = until(lambda: psql(f"select count(*) from event_outbox where event_type='jurisdiction.created' and partition_key='{jid}' and published_at is not null") == "1", 40, 2)
    check("outbox: jurisdiction.created committed with the write and published by the relay (J-11, J-15)", published)
    r = subprocess.run(["docker", "exec", KAFKA, "/opt/kafka/bin/kafka-console-consumer.sh", "--bootstrap-server", "localhost:9092",
                        "--topic", TOPIC, "--from-beginning", "--timeout-ms", "15000"], capture_output=True)
    out = r.stdout.decode("utf8", "replace")
    if not out:
        r = subprocess.run(["docker", "exec", KAFKA, "kafka-console-consumer", "--bootstrap-server", "localhost:9092",
                            "--topic", TOPIC, "--from-beginning", "--timeout-ms", "15000"], capture_output=True)
        out = r.stdout.decode("utf8", "replace")
    found = [line for line in out.splitlines() if jid in line and "jurisdiction.created" in line]
    if check(f"kafka: jurisdiction.created on {TOPIC}", bool(found), f"{len(out.splitlines())} messages read"):
        env = json.loads(found[0])
        check("kafka: envelope carries the payload hash and the aggregate version (J-12)",
              str(env.get("payload_hash", "")).startswith("sha256:") and env.get("aggregate_version") == 1, brief({k: env.get(k) for k in ("payload_hash", "aggregate_version")}))
    return jid


def rules_and_approval(jid):
    body = {"rule_domain": "TAX", "rule_code": f"E2E{RUN}".upper(), "rule_name": "e2e standard rate",
            "effective_from": "2025-06-01T00:00:00Z", "rule_payload": {"applies": ["B2C"]}}
    s, _, rule = jr("POST", f"/v1/admin/jurisdictions/{jid}/rules", body=body)
    if not check("rules: the admin authors a DRAFT rule (201)", s == 201 and rule.get("rule_status") == "DRAFT", f"HTTP {s} {brief(rule)}"):
        return None
    rid = rule["jurisdiction_rule_id"]
    check("events: regulatory-rule.versioned recorded with the rule (s26)",
          until(lambda: psql(f"select count(*) from event_outbox where event_type='regulatory-rule.versioned' and partition_key='{rid}'") == "1", 10))
    s, _, b = jr("POST", f"/v1/admin/rules/{rid}/transition", body={"new_status": "ACTIVE"}, headers={"X-Approval-Reference": str(uuid.uuid4())})
    check("rules: the author cannot activate their own rule (403)", s == 403, f"HTTP {s} {b.get('code')}")
    s, _, b = jr("POST", f"/v1/admin/rules/{rid}/transition", principal=APPROVER, body={"new_status": "ACTIVE"},
                 headers={"X-Approval-Reference": str(uuid.uuid4())})
    if WORKFLOW_LIVE:
        check("workflow-svc (J-02): an approval workflow-svc does not know is refused (403 approval_not_valid)",
              s == 403 and b.get("code") == "approval_not_valid", f"HTTP {s} {b.get('code')}")
    else:
        check("workflow-svc (J-02): workflow-svc down -> activation fails closed (503 approval_service_unavailable)",
              s == 503 and b.get("code") == "approval_service_unavailable", f"HTTP {s} {b.get('code')}")
    s, _, cur = jr("GET", f"/v1/rules/{rid}")
    check("rules: a refused activation changes nothing (still DRAFT)", cur.get("rule_status") == "DRAFT", cur.get("rule_status"))
    if WORKFLOW_LIVE:
        workflow_outcomes(rid)
    else:
        skip("workflow-svc (J-02): live approve and refuse outcomes", "--no-workflow")
    return rid


# ── workflow-svc: the approval a rule activation cites (J-02) ───────────────

def open_approval(subject_id):
    """An approval workflow opened by the demo admin, one SINGLE stage for the approver."""
    s, _, wf = jr("POST", "/v1/workflows", base=WF, body={
        "tenant_id": TENANT, "legal_entity_id": ENTITY, "workflow_type": "JURISDICTION_RULE_ACTIVATION",
        "subject_type": "jurisdiction_rule", "subject_id": subject_id,
        "stages": [{"stage_type": "SINGLE", "approver_principal_id": APPROVER}]})
    return (wf.get("workflow_instance_id") if s in (200, 201) else None), f"HTTP {s} {brief(wf)}"


def decide(wid, action="APPROVE"):
    """The approver decides the workflow (If-Match from its current ETag, API-3)."""
    _, h, _ = jr("GET", f"/v1/workflows/{wid}", principal=APPROVER, base=WF)
    extra = {"If-Match": h.get("ETag")} if hasattr(h, "get") and h.get("ETag") else {}
    s, _, b = jr("POST", f"/v1/workflows/{wid}/actions", principal=APPROVER, base=WF, headers=extra,
                 body={"action": action, "rationale": f"e2e: rule reviewed ({action.lower()})"})
    return s in (200, 201), f"HTTP {s} {brief(b)}"


def approve_rule(rid):
    """An APPROVED workflow bound to rule rid, decided by the approver: (workflow id, why)."""
    wid, why = open_approval(rid)
    if not wid:
        return None, "open: " + why
    ok, why = decide(wid)
    return (wid, "") if ok else (None, "approve: " + why)


def activate(rid, wid):
    return jr("POST", f"/v1/admin/rules/{rid}/transition", principal=APPROVER, body={"new_status": "ACTIVE"},
              headers={"X-Approval-Reference": wid})


def refused(name, rid, wid, why):
    if not check(f"setup: {name}", wid, why):
        return
    s, _, b = activate(rid, wid)
    check(f"workflow-svc (J-02): activation citing {name} is refused (403 approval_not_valid)",
          s == 403 and b.get("code") == "approval_not_valid", f"HTTP {s} {b.get('code')} {b.get('detail', '')}")


def workflow_outcomes(rid):
    wid, why = open_approval(rid)
    refused("a PENDING workflow of this rule", rid, wid, why)

    wid, why = open_approval(rid)
    if wid:
        ok, why = decide(wid, "REJECT")
        wid = wid if ok else None
    refused("a REJECTED workflow of this rule", rid, wid, why)

    wid, why = approve_rule(str(uuid.uuid4()))
    refused("an APPROVED workflow of another rule", rid, wid, why)

    s, _, cur = jr("GET", f"/v1/rules/{rid}")
    check("rules: after the three refusals the rule is still DRAFT", cur.get("rule_status") == "DRAFT", cur.get("rule_status"))

    wid, why = approve_rule(rid)
    if not check("workflow-svc: an approval of this rule, decided by the approver (APPROVED)", wid, why):
        return
    s, _, b = activate(rid, wid)
    check("workflow-svc (J-02): activation citing the APPROVED workflow succeeds (200 ACTIVE)",
          s == 200 and b.get("rule_status") == "ACTIVE", f"HTTP {s} {b.get('code') or b.get('rule_status')}")
    s, _, hist = jr("GET", f"/v1/rules/{rid}/status-history")
    rows = hist if isinstance(hist, list) else hist.get("items", [])
    open_row = next((r for r in rows if r.get("known_to") is None), {})
    check("rules: the status history records the ACTIVE row with the approval reference and the activator",
          open_row.get("rule_status") == "ACTIVE" and open_row.get("change_reason") == wid and open_row.get("changed_by_principal_id") == APPROVER,
          brief(open_row))
    check("events: jurisdiction.rule.activated recorded with the activation",
          until(lambda: psql(f"select count(*) from event_outbox where event_type='jurisdiction.rule.activated' and partition_key='{rid}'") == "1", 10))


def pack_lifecycle(jid, jcode, signing):
    s, _, regime = jr("POST", "/v1/admin/regimes", body={"regime_code": "VAT", "regime_name": "Value added tax"})
    regime_id = regime.get("regime_id")
    check("pack: regime available", s in (200, 201) and regime_id, f"HTTP {s}")
    src_body = {"jurisdiction_id": jid, "authority": "E2E authority", "source_type": "STATUTE", "authority_level": "BINDING_LAW",
                "title": f"E2E Act {RUN}", "location": f"https://example.invalid/e2e/{RUN}",
                "snapshot_hash": "sha256:" + uuid.uuid4().hex + uuid.uuid4().hex, "published_on": "2025-01-01", "effective_on": "2025-01-01"}
    s, _, src = jr("POST", "/v1/admin/sources", body=src_body)
    sid = src.get("source_id")
    check("pack: the admin captures a source (201)", s == 201 and sid, f"HTTP {s} {brief(src)}")
    s, h, _ = jr("GET", f"/v1/sources/{sid}")
    check("API-3: the source read answers its version as ETag", s == 200 and h.get("ETag") == '"1"', f"ETag={h.get('ETag')}")
    s, _, b = jr("POST", f"/v1/admin/sources/{sid}/review", principal=ADMIN)
    check("maker-checker: the author cannot review their own source", s in (403, 409, 422), f"HTTP {s} {b.get('code')}")
    s, _, b = jr("POST", f"/v1/admin/sources/{sid}/review", principal=APPROVER, headers={"If-Match": '"7"'})
    check("API-3: a stale If-Match is refused (412) and changes nothing", s == 412 and b.get("code") == "precondition_failed", f"HTTP {s} {b.get('code')}")
    s, h, b = jr("POST", f"/v1/admin/sources/{sid}/review", principal=APPROVER, headers={"If-Match": '"1"'})
    check("maker-checker: a second verified principal reviews the source (ETag moves to 2)", s == 200 and h.get("ETag") == '"2"', f"HTTP {s} ETag={h.get('ETag')} {b.get('code')}")
    s, _, interp = jr("POST", "/v1/admin/interpretations", body={"jurisdiction_id": jid, "regime_id": regime_id, "subject": "e2e",
                                                                 "decision": "standard rate applies", "rationale": "e2e", "source_ids": [sid]})
    iid = interp.get("interpretation_id")
    s2, _, _ = jr("POST", f"/v1/admin/interpretations/{iid}/approve", principal=APPROVER)
    check("maker-checker: interpretation recorded by the admin, approved by the approver", s == 201 and s2 == 200, f"HTTP {s}/{s2}")
    body = {"rule_domain": "TAX", "rule_code": "STD", "rule_name": "standard", "effective_from": "2026-01-01T00:00:00Z",
            "rule_payload": {"applies": ["B2C"], "rate": "0.2000"}}
    s, _, rule = jr("POST", f"/v1/admin/jurisdictions/{jid}/rules", body=body)
    rid = rule.get("jurisdiction_rule_id")
    s2, _, b = jr("PUT", f"/v1/admin/rules/{rid}/provenance", body={"regime_id": regime_id, "interpretation_id": iid, "precedence": 1,
                                                                    "published_on": "2026-01-01", "source_ids": [sid]})
    check("pack: a rule with its provenance", s == 201 and s2 == 200, f"HTTP {s}/{s2} {b.get('code')}")
    ref = f"jur.e2e{RUN}.tax.core"
    s, _, _ = jr("POST", "/v1/admin/packs", body={"pack_ref": ref, "pack_name": "E2E core", "owner": "Tax Engineering"})
    manifest = {"pack_id": ref, "pack_version": "2026.08.1", "jurisdiction_ids": [jcode], "regimes": ["VAT"], "effective_from": "2026-08-01",
                "dependencies": [{"ref": "ref.iso4217", "version": "2026.06"}], "source_register_version": 14, "rule_modules": [rid]}
    s2, _, _ = jr("POST", f"/v1/admin/packs/{ref}/versions", body=manifest)
    s3, _, _ = jr("POST", f"/v1/admin/packs/{ref}/versions/2026.08.1/submit-review")
    check("pack: pack, version and submit for review", s == 201 and s2 == 201 and s3 == 200, f"HTTP {s}/{s2}/{s3}")

    s, h, op = jr("POST", f"/v1/admin/packs/{ref}/versions/2026.08.1/compile", headers={"Prefer": "respond-async"})
    loc = h.get("Location") if hasattr(h, "get") else None
    if check("API-5: compile with Prefer: respond-async answers 202 + Location", s == 202 and loc and loc.startswith("/v1/operations/"), f"HTTP {s} {loc}"):
        done = until(lambda: (lambda r: r if r[2].get("state") in ("succeeded", "failed", "cancelled") else None)(jr("GET", loc)), 30)
        st = done[2] if done else {}
        check("API-5: the operation succeeds and names the artifact", st.get("state") == "succeeded" and st.get("resource_ref", "").endswith("/artifact"), brief(st))
        # Headers only: a verified envelope names its own tenant, which wins over X-Tenant-Id.
        s, _, b = jr("GET", f"/v1/operations/{op.get('id')}", tenant=OTHER_TENANT, verified=False)
        check("API-5: another tenant cannot read the operation (404)", s == 404, f"HTTP {s}")
    s, _, art = jr("GET", f"/v1/packs/{ref}/versions/2026.08.1/artifact")
    check("pack: the compiled artifact is readable", s == 200 and art.get("artifact_digest") or (art.get("artifact") or {}).get("artifact_digest"), f"HTTP {s}")

    if not signing:
        check("pack: sign and verify with the dev signing key (run --prepare-signing-key and restart the service)", False,
              "the service runs without PACK_SIGNING_KEY_FILE")
        return
    seed = base64.b64decode(open(os.path.join(KEY_DIR, "pack-signing.key")).read().strip())
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    pub = Ed25519PrivateKey.from_private_bytes(seed).public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    s, _, b = jr("POST", "/v1/admin/pack-signing-keys", body={"key_ref": KEY_REF, "public_key": base64.b64encode(pub).decode()})
    check("pack: the dev signing key is registered (201, or 200 already)", s in (200, 201), f"HTTP {s} {b.get('code')}")
    s, _, b = jr("POST", f"/v1/admin/packs/{ref}/versions/2026.08.1/sign")
    check("pack: sign the artifact", s == 200, f"HTTP {s} {b.get('code')}")
    s, _, v = jr("POST", f"/v1/packs/{ref}/versions/2026.08.1/verify")
    check("pack: the signed artifact verifies (digest and signature)", s == 200 and (v.get("verified") is True or (v.get("verification") or {}).get("verified") is True), f"HTTP {s} {brief(v)}")


def paging():
    s, _, p1 = jr("GET", "/v1/jurisdictions?page_size=1&filter[type]=COUNTRY")
    if not check("API-4: a paged list answers {items, next_cursor, page_size}", s == 200 and len(p1.get("items", [])) == 1 and p1.get("next_cursor"), f"HTTP {s}"):
        return
    c = urllib.parse.quote(p1["next_cursor"])
    s, _, p2 = jr("GET", f"/v1/jurisdictions?page_size=1&filter[type]=COUNTRY&cursor={c}")
    check("API-4: the next page is a different jurisdiction", s == 200 and p2["items"] and p2["items"][0]["jurisdiction_id"] != p1["items"][0]["jurisdiction_id"], f"HTTP {s}")
    s, _, b = jr("GET", f"/v1/jurisdictions?page_size=1&filter[type]=COUNTRY&cursor={c}", tenant=OTHER_TENANT, verified=False)
    check("API-4: another tenant's cursor is refused (400 invalid_cursor)", s == 400 and b.get("code") == "invalid_cursor", f"HTTP {s} {b.get('code')}")
    s, _, b = jr("GET", "/v1/jurisdictions?page_size=1&type=COUNTRY")
    check("API-4: a bare filter in a paged request is refused (400 unknown_filter)", s == 400 and b.get("code") == "unknown_filter", f"HTTP {s} {b.get('code')}")


E2E_ENTITY_ROLE = "e2e00000-0000-4000-8000-00000000f082"


def grant_entity_create():
    """DEVELOPMENT ONLY: ENTITY_CREATE in tenant-entity-registry for the demo admin
    (the demo seed has no bundle for that service), as the other e2e scripts do."""
    sql = f"""
INSERT INTO roles (role_id, tenant_id, role_code, role_name, role_scope_type, active_flag, created_by_principal_id)
VALUES ('{E2E_ENTITY_ROLE}', '{TENANT}', 'E2E_JURISDICTION_ENTITY_CREATOR', 'e2e: entity creator for the jurisdiction edge', 'LEGAL_ENTITY', true, 'e2e')
ON CONFLICT DO NOTHING;
INSERT INTO permission_bundles (role_id, bundle_code, permitted_actions, active_flag)
VALUES ('{E2E_ENTITY_ROLE}', 'E2E_ENTITY_CREATE', '["ENTITY_CREATE"]'::jsonb, true)
ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions = EXCLUDED.permitted_actions, active_flag = true;
INSERT INTO principal_role_assignments (principal_id, role_id, legal_entity_id, effective_from, assigned_by)
SELECT '{ADMIN}', '{E2E_ENTITY_ROLE}', '{ENTITY}'::uuid, now() - interval '1 minute', 'e2e'
WHERE NOT EXISTS (SELECT 1 FROM principal_role_assignments a WHERE a.principal_id = '{ADMIN}' AND a.role_id = '{E2E_ENTITY_ROLE}');
"""
    r = subprocess.run(["docker", "exec", "-i", PG, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "authorization_svc", "-c", sql],
                       capture_output=True)
    return r.returncode == 0, r.stderr.decode("utf8", "replace").strip()


def entity_edge(jid):
    ok, why = grant_entity_create()
    check("setup: demo admin may create entities in tenant-entity-registry (dev-only grant)", ok, why)
    time.sleep(6)  # authorization-svc's decision cache
    body = {"tenant_id": TENANT, "entity_code": f"E2E-{RUN}", "legal_name": f"E2E Jurisdiction Holdings {RUN}", "entity_type": "SUBSIDIARY",
            "default_currency_code": "GBP", "fiscal_calendar_id": FISCAL_CALENDAR, "primary_jurisdiction_id": jid,
            "data_residency_policy_id": RESIDENCY_POLICY, "correlation_id": f"e2e-jur-{RUN}",
            # ORG-03 s4.3 required source inputs.
            "registered_office": {"line1": "1 E2E Street", "city": "London", "postal_code": "EC1A 1AA", "country_code": "GB"},
            "source_evidence_ref": f"e2e:registry-extract:{RUN}"}
    s, _, e = jr("POST", "/v1/entities", body=body, base=TER)
    eid = e.get("legal_entity_id")
    if not check("tenant-entity-registry: an entity whose primary jurisdiction is this service's (checked live)", s == 201 and eid, f"HTTP {s} {brief(e)}"):
        return
    projected = until(lambda: (lambda r: r if r[0] == 200 and r[2].get("jurisdictions") else None)(jr("GET", f"/v1/entities/{eid}/jurisdictions")), 45, 2)
    check("kafka consumer: entity.created projected into the entity's jurisdictions", bool(projected),
          brief(projected[2]) if projected else "nothing within 45s")
    s, _, other = jr("GET", f"/v1/entities/{eid}/jurisdictions", tenant=OTHER_TENANT, verified=False)
    check("RLS: another tenant does not see the entity's jurisdictions", s == 200 and not other.get("jurisdictions"), f"HTTP {s} {brief(other)}")
    psql(f"delete from entity_jurisdiction_applicability where legal_entity_id='{eid}'")
    s, _, gone = jr("GET", f"/v1/entities/{eid}/jurisdictions")
    check("setup: the entity's mapping is lost (as if its events left Kafka retention)", s == 200 and not gone.get("jurisdictions"))
    s, _, r = jr("POST", "/v1/admin/entity-applicability:backfill", body={"legal_entity_id": eid})
    check("J-10: the backfill reads the registry and restores the mapping", s == 200 and r.get("applied", 0) >= 1, f"HTTP {s} {brief(r)}")
    s, _, back = jr("GET", f"/v1/entities/{eid}/jurisdictions")
    check("J-10: the entity's primary jurisdiction is projected again", s == 200 and any(x.get("jurisdiction_id") == jid for x in back.get("jurisdictions", [])), brief(back))
    s, _, r = jr("POST", "/v1/admin/entity-applicability:backfill", body={"legal_entity_id": eid})
    check("J-10: a rerun changes nothing", s == 200 and r.get("applied") == 0, f"HTTP {s} {brief(r)}")


def outbox_requeue():
    pk = f"e2e-parked-{RUN}"
    psql(f"insert into event_outbox (event_type, event_payload, topic, partition_key, publish_attempts, last_error) "
         f"values ('e2e.probe', '{{\"event_id\":\"{RUN}\"}}', '{TOPIC}', '{pk}', 10, 'e2e: refused by the broker')")
    oid = psql(f"select outbox_id from event_outbox where partition_key='{pk}'")
    s, _, page = jr("GET", "/v1/admin/outbox/parked?page_size=200")
    check("J-16: the parked event is listed with its last error", s == 200 and any(i.get("outbox_id") == oid for i in page.get("items", [])), f"HTTP {s}")
    s, _, b = jr("POST", f"/v1/admin/outbox/{oid}/requeue", body={"reason": "e2e: broker fixed", "evidence_ref": f"E2E-{RUN}"})
    check("J-16: requeued with a reason (200)", s == 200 and b.get("attempts_before") == 10, f"HTTP {s} {brief(b)}")
    check("J-16: the relay publishes the requeued event",
          until(lambda: psql(f"select count(*) from event_outbox where outbox_id='{oid}' and published_at is not null") == "1", 40, 2))
    check("J-16: the requeue is evidence (append-only)", "append-only" in subprocess.run(
        ["docker", "exec", "-i", PG, "psql", "-U", "postgres", "-d", "jurisdiction_rules", "-c", f"delete from outbox_requeues where outbox_id='{oid}'"],
        capture_output=True).stderr.decode())


def effectivity(jid):
    # The image is distroless (no printenv): read the container's configuration instead.
    interval = container_env().get("RULE_EFFECTIVITY_INTERVAL_SECONDS") or "60"
    # Far enough ahead that the approval and the activation finish before the start.
    lead = 40 if WORKFLOW_LIVE else 15
    start = datetime.now(timezone.utc) + timedelta(seconds=lead)
    body = {"rule_domain": "TAX", "rule_code": f"EFF{RUN}".upper(), "rule_name": f"starts in {lead} seconds",
            "effective_from": start.strftime("%Y-%m-%dT%H:%M:%S.%fZ"), "rule_payload": {}}
    s, _, rule = jr("POST", f"/v1/admin/jurisdictions/{jid}/rules", body=body)
    rid = rule.get("jurisdiction_rule_id")
    if WORKFLOW_LIVE:
        wid, why = approve_rule(rid)
        s2, _, b = activate(rid, wid) if wid else (0, {}, {"code": why})
        check("setup: the future-dated rule is activated through the API (workflow-svc approval, J-02)",
              s2 == 200 and b.get("rule_status") == "ACTIVE" and datetime.now(timezone.utc) < start,
              f"HTTP {s2} {b.get('code') or b.get('rule_status')}")
    else:
        # Without workflow-svc the API cannot activate (J-02). The watcher only
        # reads rule_status, so the fixture activates the rule in the database.
        psql(f"update jurisdiction_rules set rule_status='ACTIVE', updated_by_principal_id='e2e-fixture' where jurisdiction_rule_id='{rid}'")
        check("setup: the future-dated rule is ACTIVE (database fixture, --no-workflow)",
              psql(f"select rule_status from jurisdiction_rules where jurisdiction_rule_id='{rid}'") == "ACTIVE")
    wait = lead + int(interval) * 2 + 15
    check(f"s26: regulatory-rule.effective announced when the start passes (watcher every {interval}s)",
          s == 201 and until(lambda: psql(f"select count(*) from event_outbox where event_type='regulatory-rule.effective' and partition_key='{rid}'") == "1", wait, 2),
          f"HTTP {s}")


FEED_ACTOR = "jurisdiction-rules-svc:regulatory-feed"


def kafka_tool(tool, *args, stdin=None):
    """Run a Kafka CLI tool in the broker container (apache/kafka, then confluent paths)."""
    for exe in (f"/opt/kafka/bin/{tool}.sh", tool):
        r = subprocess.run(["docker", "exec", "-i", KAFKA, exe, "--bootstrap-server", "localhost:9092", *args],
                           input=stdin, capture_output=True)
        if r.returncode == 0 or b"executable file not found" not in r.stderr + r.stdout:
            return r
    return r


def publish_feed_event(topic, event):
    return kafka_tool("kafka-console-producer", "--topic", topic, stdin=(json.dumps(event) + "\n").encode())


def regulatory_feed(jid):
    topic = container_env().get("KAFKA_REGULATORY_FEED_TOPIC", "")
    if not topic:
        skip("regulatory feed (JR-6): live consumer", "the service runs without KAFKA_REGULATORY_FEED_TOPIC")
        return
    if not WORKFLOW_LIVE:
        skip("regulatory feed (JR-6): live consumer", "--no-workflow (the consumer only marks ACTIVE rules)")
        return
    # The real producer would create the topic on first publish; create it first
    # so the consumer group gets its partition while the rule is approved.
    r = kafka_tool("kafka-topics", "--create", "--if-not-exists", "--topic", topic, "--partitions", "1", "--replication-factor", "1")
    check(f"setup: feed topic {topic} exists", r.returncode == 0, r.stderr.decode("utf8", "replace")[:200])

    ref = f"e2e-feed:{RUN}"
    payload = {"applies": ["B2C"], "rate": "0.0500"}
    s, _, rule = jr("POST", f"/v1/admin/jurisdictions/{jid}/rules", body={
        "rule_domain": "TAX", "rule_code": f"FEED{RUN}".upper(), "rule_name": "reduced rate (feed watched)",
        "effective_from": "2025-03-01T00:00:00Z", "rule_payload": payload, "external_feed_reference": ref})
    rid = rule.get("jurisdiction_rule_id")
    if not check("feed: a rule citing an external feed reference (201)", s == 201 and rule.get("external_feed_reference") == ref,
                 f"HTTP {s} {brief(rule)}"):
        return
    wid, why = approve_rule(rid)
    s, _, b = activate(rid, wid) if wid else (0, {}, {"code": why})
    if not check("feed: the rule is activated through workflow-svc (ACTIVE)", s == 200 and b.get("rule_status") == "ACTIVE",
                 f"HTTP {s} {b.get('code') or b.get('rule_status')}"):
        return

    event = {"event_id": str(uuid.uuid4()), "event_type": "regulatory.source.changed", "correlation_id": f"e2e-jur-feed-{RUN}",
             "payload": {"external_feed_reference": ref, "authority": "E2E Revenue Authority",
                         "change_summary": "reduced rate amended", "observed_at": iso(datetime.now(timezone.utc)),
                         "source_url": f"https://example.invalid/e2e/{RUN}/notice"}}
    r = publish_feed_event(topic, event)
    check("kafka: regulatory.source.changed published to the feed topic", r.returncode == 0, r.stderr.decode("utf8", "replace")[:200])

    marked = until(lambda: (lambda x: x if x[2].get("legal_drift_state") == "UNDER_REVIEW" else None)(jr("GET", f"/v1/rules/{rid}")), 60, 2)
    if not check("JR-6: the consumer moves the ACTIVE rule citing the reference to UNDER_REVIEW", bool(marked),
                 "still " + str(jr("GET", f"/v1/rules/{rid}")[2].get("legal_drift_state")) if not marked else ""):
        return
    cur = marked[2]
    check("JR-6: the drift never changes the rule's status or content (ACTIVE, same payload)",
          cur.get("rule_status") == "ACTIVE" and cur.get("rule_payload") == payload, brief({k: cur.get(k) for k in ("rule_status", "rule_payload")}))
    s, _, ev = jr("GET", f"/v1/rules/{rid}/drift-events")
    evs = ev if isinstance(ev, list) else ev.get("items", [])
    feed_evs = [e for e in evs if e.get("recorded_by_principal_id") == FEED_ACTOR]
    check("JR-6: the drift event is recorded by the feed, quoting the authority and the event id",
          len(feed_evs) == 1 and feed_evs[0].get("to_state") == "UNDER_REVIEW" and event["event_id"] in (feed_evs[0].get("reason") or "")
          and "E2E Revenue Authority" in (feed_evs[0].get("reason") or ""), brief(feed_evs[:1]))
    check("events: legal.drift.detected recorded for the rule",
          until(lambda: psql(f"select count(*) from event_outbox where event_type='legal.drift.detected' and partition_key='{rid}'") == "1", 10),
          psql(f"select count(*) from event_outbox where event_type='legal.drift.detected' and partition_key='{rid}'"))
    check("JR-6: the event is claimed once (consumed_events)",
          psql(f"select count(*) from consumed_events where event_id='{event['event_id']}'") == "1")

    # A person resolves the drift; the same event redelivered must not reopen it.
    s, _, b = jr("POST", f"/v1/admin/rules/{rid}/drift", body={"drift_state": "CURRENT", "reason": "e2e: reviewed, rule still correct"})
    check("drift: a person records the review outcome (CURRENT)", s == 200 and b.get("legal_drift_state", (b.get("rule") or {}).get("legal_drift_state")) == "CURRENT",
          f"HTTP {s} {brief(b)}")
    r = publish_feed_event(topic, event)
    time.sleep(8)  # the consumer polls every 500 ms; give it time to (not) act
    _, _, cur = jr("GET", f"/v1/rules/{rid}")
    _, _, ev = jr("GET", f"/v1/rules/{rid}/drift-events")
    evs = ev if isinstance(ev, list) else ev.get("items", [])
    check("JR-6: the same event redelivered changes nothing (still CURRENT, one feed drift event, one legal.drift.detected)",
          r.returncode == 0 and cur.get("legal_drift_state") == "CURRENT"
          and len([e for e in evs if e.get("recorded_by_principal_id") == FEED_ACTOR]) == 1
          and psql(f"select count(*) from event_outbox where event_type='legal.drift.detected' and partition_key='{rid}'") == "1",
          f"state={cur.get('legal_drift_state')}")


def rls_runtime_role():
    out = psql("begin; set local role app_jurisdiction_rules; select count(*) from entity_jurisdiction_applicability; commit;")
    check("RLS: the runtime role with no tenant set reads no tenant rows", out.strip().splitlines()[-1:] == ["0"] if out else False, out)


def prepare_signing_key():
    os.makedirs(KEY_DIR, exist_ok=True)
    path = os.path.join(KEY_DIR, "pack-signing.key")
    if not os.path.exists(path):
        open(path, "w").write(base64.b64encode(os.urandom(32)).decode() + "\n")
    print(f"dev signing key: {os.path.abspath(path)}")
    print("restart the service with it (from deployments/):")
    print(f"  MSYS_NO_PATHCONV=1 JURISDICTION_PACK_SIGNING_KEY_REF={KEY_REF} JURISDICTION_PACK_SIGNING_KEY_FILE=/keys/pack-signing.key "
          "JURISDICTION_RULE_EFFECTIVITY_INTERVAL_SECONDS=5 JURISDICTION_REGULATORY_FEED_TOPIC=zoiko.regulatory.feed.e2e docker compose -p deployments -f docker-compose.yml up -d --no-deps --no-build --force-recreate jurisdiction-svc")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--prepare-signing-key", action="store_true")
    ap.add_argument("--no-workflow", action="store_true", help="workflow-svc is not running: expect the fail-closed 503")
    a = ap.parse_args()
    if a.prepare_signing_key:
        prepare_signing_key()
        return 0
    signing = container_env().get("PACK_SIGNING_KEY_FILE", "").startswith("/") and os.path.exists(os.path.join(KEY_DIR, "pack-signing.key"))

    global WORKFLOW_LIVE
    WORKFLOW_LIVE = not a.no_workflow
    print(f"== jurisdiction-rules-svc dependency e2e (run {RUN})")
    preflight()
    if WORKFLOW_LIVE:
        s, _, _ = call("GET", f"{WF}/healthz", timeout=5)
        if not check("workflow-svc: /healthz (run with --no-workflow to leave it out)", s == 200, f"HTTP {s}"):
            WORKFLOW_LIVE = False
    identity_and_authz()
    jid = registry_and_kafka()
    if jid:
        jcode = f"E2E{RUN}".upper()
        rules_and_approval(jid)
        pack_lifecycle(jid, jcode, signing)
        paging()
        entity_edge(jid)
        effectivity(jid)
        regulatory_feed(jid)
    outbox_requeue()
    rls_runtime_role()

    failed = [r for r in results if r[1] == "FAIL"]
    skipped = [r for r in results if r[1] == "SKIP"]
    print(f"\n== {len(results) - len(failed) - len(skipped)} passed, {len(failed)} failed, {len(skipped)} skipped, {len(results)} checks")
    for name, _, detail in skipped:
        print(f"   SKIP {name}  {detail}")
    for name, _, detail in failed:
        print(f"   FAIL {name}  {detail}")
    return len(failed)


if __name__ == "__main__":
    sys.exit(main())
