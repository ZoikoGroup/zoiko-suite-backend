#!/usr/bin/env python3
"""Live proof of the ZS-SVC-Y-001 control plane in notification-svc.

Drives the running service over HTTP (default http://localhost:8133) through
NCD-01 … NCD-05 and the §3.3/§3.4 rules, and reads the database through
`docker exec zoiko-postgres psql` for the facts only the database can show
(immutability triggers, the outbox, RLS). Every check prints PASS/FAIL; the
exit code is the number of failures.

Needs: the service up with NCD_CALLBACK_SECRET_SMTP_PRIMARY and
NOTIFICATION_WEBHOOK_SECRET set (the values below), an authorization service
that grants every principal except "mallory", and an identity service that
knows every principal except "ghost". scripts/README-live-check.md says how.

    python scripts/ncd_live_check.py [--base URL]
"""
import argparse, hashlib, hmac, json, subprocess, sys, time, urllib.error, urllib.parse, urllib.request, uuid

CALLBACK_SECRET = "live-callback-secret"
WEBHOOK_SECRET = "live-webhook-secret"

ap = argparse.ArgumentParser()
ap.add_argument("--base", default="http://localhost:8133")
ap.add_argument("--db-container", default="zoiko-postgres")
ap.add_argument("--db", default="notification")
args = ap.parse_args()
sys.stdout.reconfigure(encoding="utf-8")
BASE = args.base
TENANT = "live-" + uuid.uuid4().hex[:8]
RUN = uuid.uuid4().hex[:8]
LE = "le-live"
failures = 0
passes = 0


def check(name, ok, detail=""):
    global failures, passes
    if ok:
        passes += 1
        print(f"PASS  {name}")
    else:
        failures += 1
        print(f"FAIL  {name}  {detail}")


def req(method, path, principal="alice", body=None, tenant=None, key=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    r = urllib.request.Request(BASE + path, data=data, method=method)
    h = {"Content-Type": "application/json", "X-Tenant-Id": tenant or TENANT, "X-Principal-Id": principal,
         "X-Request-Id": uuid.uuid4().hex, "X-Correlation-ID": "live-" + uuid.uuid4().hex[:8],
         "X-Source-Channel": "api", "X-Legal-Entity-Id": LE}
    if method != "GET":
        h["Idempotency-Key"] = key or uuid.uuid4().hex
    h.update(headers or {})
    for k, v in h.items():
        if v is not None:
            r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=20) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {}), dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw), dict(e.headers)
        except Exception:
            return e.code, {"raw": raw.decode(errors="replace")}, dict(e.headers)


def sql(q):
    out = subprocess.run(["docker", "exec", args.db_container, "psql", "-U", "postgres", "-d", args.db, "-tAc", q],
                         capture_output=True, text=True)
    return (out.stdout.strip(), out.stderr.strip())


def wait_for(fn, timeout=15):
    end = time.time() + timeout
    while time.time() < end:
        v = fn()
        if v:
            return v
        time.sleep(0.5)
    return fn()


# ── NCD-01 ──────────────────────────────────────────────────────────────────
def intent(code, **kw):
    body = {"legal_entity_id": LE, "intent_code": code, "display_name": code, "purpose_class": "TRANSACTIONAL_RELATIONSHIP",
            "domain_owner": "billing", "sensitivity": "S1", "urgency": "U1", "evidence_class": "E1",
            "allowed_channels": ["EMAIL", "IN_APP"], "fallback_allowed": True,
            "variable_contract": [{"name": "invoice_no", "type": "string", "required": True, "sensitivity": "S1"}]}
    body.update(kw)
    s, b, _ = req("POST", "/v1/communication-intents", body=body)
    assert s == 201, (s, b)
    s2, b2, _ = req("POST", f"/v1/communication-intents/{b['intent_id']}/activate", principal="bob", body={"version": 1})
    assert s2 == 200, (s2, b2)
    return b2


def publish(intent_id, channel, subject, body, locale="en-GB"):
    s, tv, _ = req("POST", "/v1/templates", body={"intent_id": intent_id, "channel": channel, "locale": locale, "subject": subject, "body": body})
    assert s == 201, (s, tv)
    tid = tv["template_version_id"]
    assert req("POST", f"/v1/templates/{tid}/validate")[0] == 200
    assert req("POST", f"/v1/templates/{tid}/approve", principal="bob")[0] == 200
    s, tv, _ = req("POST", f"/v1/templates/{tid}/publish", principal="bob")
    assert s == 200, (s, tv)
    return tv


print(f"tenant {TENANT} against {BASE}\n── NCD-01 intent, template & content registry")
s, b, _ = req("POST", "/v1/communication-intents", principal="mallory", body={"legal_entity_id": LE})
check("unauthorized author is refused (403)", s == 403, s)
s, i1, _ = req("POST", "/v1/communication-intents", key="intent-k1", body={"legal_entity_id": LE, "intent_code": "inv", "display_name": "Invoice",
    "purpose_class": "TRANSACTIONAL_RELATIONSHIP", "domain_owner": "billing", "sensitivity": "S1", "urgency": "U1", "evidence_class": "E1",
    "allowed_channels": ["EMAIL", "IN_APP"], "fallback_allowed": True,
    "variable_contract": [{"name": "invoice_no", "type": "string", "required": True, "sensitivity": "S1"}]})
check("POST /v1/communication-intents creates a DRAFT with a server id", s == 201 and i1.get("status") == "DRAFT" and i1.get("version") == 1, (s, i1))
iid = i1["intent_id"]
s, rep, hdr = req("POST", "/v1/communication-intents", key="intent-k1", body={"legal_entity_id": LE, "intent_code": "inv", "display_name": "Invoice",
    "purpose_class": "TRANSACTIONAL_RELATIONSHIP", "domain_owner": "billing", "sensitivity": "S1", "urgency": "U1", "evidence_class": "E1",
    "allowed_channels": ["EMAIL", "IN_APP"], "fallback_allowed": True,
    "variable_contract": [{"name": "invoice_no", "type": "string", "required": True, "sensitivity": "S1"}]})
check("Idempotency-Key replays the first response (cross-service finding 2)", s == 201 and rep.get("intent_id") == iid and hdr.get("X-Idempotent-Replay") == "true", (s, hdr.get("X-Idempotent-Replay")))
s, b, _ = req("POST", f"/v1/communication-intents/{iid}/activate", body={"version": 1})
check("author cannot activate own intent (SoD, 403)", s == 403 and b.get("error_code") == "self_approval_forbidden", (s, b))
s, b, _ = req("POST", f"/v1/communication-intents/{iid}/activate", principal="bob", body={"version": 1})
check("a second principal activates it", s == 200 and b.get("status") == "ACTIVE", (s, b))
out, err = sql(f"UPDATE ncd_communication_intents SET purpose_class='MARKETING_PROMOTIONAL' WHERE intent_id='{iid}'")
check("an ACTIVE intent version is immutable in the database", "immutable" in err, err)

s, tv, _ = req("POST", "/v1/templates", body={"intent_id": iid, "channel": "EMAIL", "locale": "en-GB",
    "subject": "Invoice {{invoice_no}}", "body": "<p>Invoice {{invoice_no}} is ready.</p><script>x()</script>"})
tid_bad = tv["template_version_id"]
s, v, _ = req("POST", f"/v1/templates/{tid_bad}/validate")
check("validate blocks a script and stays DRAFT (422)", s == 422 and v.get("status") == "DRAFT", (s, v.get("status")))
tv_mail = publish(iid, "EMAIL", "Invoice {{invoice_no}}", "<p>Invoice {{invoice_no}} is ready.</p>")
tv_app = publish(iid, "IN_APP", "Invoice {{invoice_no}}", "<p>Invoice {{invoice_no}} is in your account.</p>")
check("POST /v1/templates → validate → approve → publish", tv_mail.get("status") == "PUBLISHED" and tv_mail.get("effective_from"), tv_mail.get("status"))
out, err = sql(f"UPDATE ncd_templates SET body='edited' WHERE template_version_id='{tv_mail['template_version_id']}'")
check("NP-05 published template edited in place is refused by the database", "immutable" in err, err)
s, p, _ = req("POST", "/v1/render-previews", body={"template_version_id": tv_app["template_version_id"]}, key=None)
check("POST /v1/render-previews is synthetic and side-effect free", s == 200 and p.get("synthetic_data") is True and "[INVOICE_NO]" in p.get("body", ""), (s, p))
s, e, _ = req("GET", f"/v1/intents/{iid}/effective")
check("GET /v1/intents/{id}/effective resolves the exact template set", s == 200 and len(e.get("templates", [])) == 2, (s, e))
s, e, _ = req("GET", f"/v1/intents/{iid}/effective?knowledge_time=2020-01-01T00:00:00Z")
check("effective resolution is bitemporal (nothing known in 2020 → NCD-002)", s == 422 and e.get("reason_code") == "NCD-002", (s, e))

# ── NCD-02 ──────────────────────────────────────────────────────────────────
print("── NCD-02 recipient, channel, preference & suppression")
s, plan, _ = req("POST", "/v1/recipient-resolution", body={"intent_id": iid, "recipient_principal_id": "pat"})
check("POST /v1/recipient-resolution returns verified endpoints with provenance",
      s == 201 and {e["provenance"] for e in plan["endpoints"]} == {"IDENTITY_CONTEXT", "PLATFORM_INBOX"} and all(e["verified"] for e in plan["endpoints"]), (s, plan))
check("the plan never exposes the raw address", "pat@example.test" not in json.dumps(plan), "")
s, b, _ = req("POST", "/v1/recipient-resolution", body={"intent_id": iid, "recipient_principal_id": "pat", "recipient_tenant_id": "other"})
check("NP-12 cross-tenant recipient → NCD-020", s == 422 and b.get("reason_code") == "NCD-020", (s, b))
s, d, _ = req("POST", "/v1/channel-decision", body={"recipient_plan_id": plan["recipient_plan_id"]})
check("POST /v1/channel-decision orders EMAIL then IN_APP with evidence requirement", s == 201 and [r["channel"] for r in d["routes"]] == ["EMAIL", "IN_APP"] and d["evidence_requirement"] == "E1", (s, d))
s, d, _ = req("POST", "/v1/channel-decision", body={"recipient_plan_id": plan["recipient_plan_id"], "privacy_permission": {"decision": "INDETERMINATE"}})
check("NP-17 INDETERMINATE privacy → REVIEW_REQUIRED", d.get("outcome") == "REVIEW_REQUIRED", d.get("outcome"))
s, pref, _ = req("POST", "/v1/preferences", principal="pat", body={"muted_channels": ["EMAIL"], "time_zone": "Europe/London", "locale": "en-GB"})
check("POST /v1/preferences sets the caller's own profile", s == 200 and pref.get("version") == 1, (s, pref))
s, b, _ = req("POST", "/v1/preferences", principal="pat", body={"marketing_consent": True, "expected_version": 1})
check("POST /v1/preferences cannot carry consent (§5.5)", s == 400, s)
s, d, _ = req("POST", "/v1/channel-decision", body={"recipient_plan_id": plan["recipient_plan_id"]})
check("NP-16 muted EMAIL is excluded for a routine message", [r["channel"] for r in d["routes"]] == ["IN_APP"], d.get("routes"))
req("POST", "/v1/preferences", principal="pat", body={"muted_channels": [], "time_zone": "Europe/London", "expected_version": 1})
s, sup, _ = req("POST", "/v1/suppressions", body={"endpoint": {"channel": "EMAIL", "address": "held@example.test"}, "channel_scope": "EMAIL",
    "reason": "HARD_BOUNCE", "source": "OPERATOR", "source_evidence_ref": "ticket-1"})
check("POST /v1/suppressions records a scoped canonical suppression", s == 201 and sup.get("reason") == "HARD_BOUNCE" and "held@" not in json.dumps(sup), (s, sup))
s, lst, _ = req("GET", "/v1/suppressions?channel=EMAIL&endpoint=held@example.test")
check("GET /v1/suppressions returns purpose/channel-scoped facts", s == 200 and len(lst.get("suppressions", [])) == 1, (s, lst))
s, b, _ = req("POST", f"/v1/suppressions/{sup['suppression_id']}/lift", body={"evidence_ref": "fixed", "reason": "address corrected"})
check("lifting a hard bounce needs maker-checker (202 + approval)", s == 202 and b.get("approval", {}).get("status") == "PENDING", (s, b))

# ── §10.1 + NCD-03 ──────────────────────────────────────────────────────────
print("── NCD-03 delivery orchestration, routing & attempts; §3.3 / §3.4")
evt = "evt-" + uuid.uuid4().hex[:8]
cbody = {"intent_id": iid, "legal_entity_id": LE, "recipient_principal_id": "pat", "locale": "en-GB",
         "variables": {"invoice_no": "INV-LIVE-1"}, "source_event_id": evt}
s, c, _ = req("POST", "/v1/communications", body=cbody)
cid = c["communication"]["communication_id"]
check("POST /v1/communications creates the logical communication", s == 201, (s, c))
s, c2, _ = req("POST", "/v1/communications", body=cbody)
check("NP-21 the same source event (new Idempotency-Key) returns it with NCD-019", s == 200 and c2["communication"]["communication_id"] == cid and c2.get("reason_code") == "NCD-019", (s, c2))
s, p, _ = req("POST", f"/v1/communications/{cid}/prepare")
check("prepare pins renders, plan and decision", s == 200 and p.get("refusal") is None and len(p.get("renders", [])) == 2 and p["communication"]["lifecycle_state"] == "PREPARED", (s, p.get("refusal")))
s, dsp, _ = req("POST", f"/v1/communications/{cid}/dispatch")
check("dispatch rechecks and queues a durable job (202)", s == 202 and dsp.get("job", {}).get("state") == "QUEUED", (s, dsp))
v = wait_for(lambda: (lambda r: r[1] if r[1].get("attempts") else None)(req("GET", f"/v1/communications/{cid}")))
a0 = v["attempts"][0]
check("the worker makes one durable attempt with token, content hash and endpoint snapshot",
      a0["channel"] == "EMAIL" and a0["state"] == "ACCEPTED" and a0["idempotency_token"] and a0["content_hash"] and a0["recipient_snapshot"].get("endpoint_hash"), a0)
check("§3.3 exposed state is PROVIDER_ACCEPTED, not a generic sent", v["claims"]["delivery_state"] == "PROVIDER_ACCEPTED" and not v["claims"]["delivered"] and "sent" not in v["communication"], v["claims"])
check("legal sufficiency is never claimed", v["claims"]["legally_served"] == "NOT_DETERMINED_BY_NCD", v["claims"])
mp = json.loads(urllib.request.urlopen("http://localhost:8025/api/v1/search?query=" + urllib.parse.quote("INV-LIVE-1"), timeout=10).read()) if True else {}
check("the email really reached the relay (mailpit)", mp.get("messages_count", mp.get("total", 0)) >= 1, mp.get("messages_count"))

# Provider callbacks (NCD-04) against this attempt.
def callback(events, secret=CALLBACK_SECRET, ts=None):
    body = json.dumps({"events": events}).encode()
    ts = ts or str(int(time.time()))
    sig = "sha256=" + hmac.new(secret.encode(), (ts + ".").encode() + body, hashlib.sha256).hexdigest()
    r = urllib.request.Request(BASE + "/v1/provider-events/smtp-primary", data=body, method="POST",
                               headers={"Content-Type": "application/json", "X-NCD-Timestamp": ts, "X-NCD-Signature": sig})
    try:
        with urllib.request.urlopen(r, timeout=10) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")

print("── NCD-04 evidence, bounce, complaint & suppression")
s, b = callback([{"event_id": RUN + "-cb-1", "event_type": "delivered", "attempt_token": a0["idempotency_token"]}], secret="wrong")
check("NP-26 invalid callback signature → 401, no change", s == 401, (s, b))
s, b = callback([{"event_id": RUN + "-cb-1", "event_type": "delivered", "attempt_token": a0["idempotency_token"]}])
check("signed delivered callback applies", s == 200 and b["results"][0]["status"] == "APPLIED", (s, b))
s, b = callback([{"event_id": RUN + "-cb-1", "event_type": "delivered", "attempt_token": a0["idempotency_token"]}])
check("NP-24 duplicate callback is deduplicated", b["results"][0]["status"] == "DUPLICATE", b)
s, b = callback([{"event_id": RUN + "-cb-0", "event_type": "accepted", "attempt_token": a0["idempotency_token"]}])
s, v, _ = req("GET", f"/v1/communications/{cid}")
check("NP-25 a late 'accepted' cannot roll DELIVERED back", v["attempts"][0]["state"] == "DELIVERED" and v["claims"]["delivery_state"] == "DELIVERED_TO_MAILBOX", v["attempts"][0]["state"])
s, b = callback([{"event_id": RUN + "-cb-2", "event_type": "opened", "attempt_token": a0["idempotency_token"]}])
s, v, _ = req("GET", f"/v1/communications/{cid}")
check("NP-38 an open pixel is never display or acknowledgment", not v["claims"]["acknowledged"] and not v["claims"]["opened_or_displayed"] and v["claims"]["open_signal_only"], v["claims"])
s, ev, _ = req("GET", f"/v1/evidence/{cid}")
check("GET /v1/evidence bundles intent, renders, plan, decisions, attempts and evidence",
      s == 200 and ev.get("intent") and ev.get("renders") and ev.get("recipient_plan") and ev.get("channel_decisions") and len(ev.get("evidence", [])) >= 4, (s, list(ev.keys())))
check("every evidence fact states what it does not prove", all(e.get("does_not_prove") for e in ev.get("evidence", [])), "")
out, err = sql(f"UPDATE ncd_delivery_evidence SET normalized_state='X' WHERE communication_id='{cid}'")
check("evidence is append-only in the database", "append-only" in err, err)

# Bounce → suppression → fallback.
evt2 = "evt-" + uuid.uuid4().hex[:8]
s, c, _ = req("POST", "/v1/communications", body={**cbody, "recipient_principal_id": "bouncy", "source_event_id": evt2})
bid = c["communication"]["communication_id"]
req("POST", f"/v1/communications/{bid}/prepare"); req("POST", f"/v1/communications/{bid}/dispatch")
v = wait_for(lambda: (lambda r: r[1] if r[1].get("attempts") else None)(req("GET", f"/v1/communications/{bid}")))
s, b = callback([{"event_id": RUN + "-bn-1", "event_type": "bounced", "attempt_token": v["attempts"][0]["idempotency_token"], "detail": "550 5.1.1 unknown user"}])
v = wait_for(lambda: (lambda r: r[1] if len(r[1].get("attempts", [])) >= 2 else None)(req("GET", f"/v1/communications/{bid}")))
check("a hard bounce falls back to IN_APP under the same communication (§6.4)",
      v and v["attempts"][0]["state"] == "BOUNCED" and v["attempts"][1]["channel"] == "IN_APP" and v["attempts"][1]["origin"] == "FALLBACK", v and v.get("attempts"))
s, lst, _ = req("GET", "/v1/suppressions?channel=EMAIL&endpoint=bouncy@example.test")
check("the hard bounce became a canonical suppression", any(x["reason"] == "HARD_BOUNCE" and x["source"] == "PROVIDER_EVENT" for x in lst.get("suppressions", [])), lst)
s, inbox, _ = req("GET", "/v1/notifications", principal="bouncy")
check("the in-app fallback is in the recipient's inbox as DELIVERED_TO_INBOX", s == 200 and any(n.get("status") == "DELIVERED_TO_INBOX" for n in inbox), inbox)

# UNKNOWN never resends: an attempt left SUBMITTING by a crash.
print("── §3.4 UNKNOWN is reconciled, never blindly resent")
out, err = sql(f"""INSERT INTO ncd_attempts (attempt_id, tenant_id, job_id, communication_id, route_index, channel, binding_id, intent_id,
    intent_version, purpose_class, recipient_principal_id, origin, idempotency_token, content_hash, render_id, recipient_snapshot, state, resolution_due_at)
    SELECT gen_random_uuid(), tenant_id, job_id, communication_id, 9, channel, binding_id, intent_id, intent_version, purpose_class,
           recipient_principal_id, 'RETRY', 'live-x-' || gen_random_uuid(), content_hash, render_id, recipient_snapshot, 'UNKNOWN',
           now() + interval '30 minutes'
    FROM ncd_attempts WHERE communication_id='{cid}' LIMIT 1""")
out2, err2 = sql(f"""INSERT INTO ncd_attempts (attempt_id, tenant_id, job_id, communication_id, route_index, channel, binding_id, intent_id,
    intent_version, purpose_class, recipient_principal_id, origin, idempotency_token, content_hash, render_id, recipient_snapshot)
    SELECT gen_random_uuid(), tenant_id, job_id, communication_id, 9, channel, binding_id, intent_id, intent_version, purpose_class,
           recipient_principal_id, 'RETRY', 'live-y-' || gen_random_uuid(), content_hash, render_id, recipient_snapshot
    FROM ncd_attempts WHERE communication_id='{cid}' LIMIT 1""")
check("an attempt left UNKNOWN (simulated lost provider answer) is recorded", err == "", err)
check("INV-13 the database refuses a new attempt while one is UNKNOWN", "INV-13" in err2, err2)
s, b, _ = req("POST", f"/v1/communications/{cid}/resend", body={"reason": "customer asked"})
check("resend while UNKNOWN → NCD-014", s == 422 and b.get("reason_code") == "NCD-014", (s, b))

# ── NCD-05 ──────────────────────────────────────────────────────────────────
print("── NCD-05 regulated notice, acknowledgment & record")
reg = intent("hr.consult", purpose_class="REGULATED_RIGHTS_AFFECTING", evidence_class="E3", ack_requirement="AUTHENTICATED_ACK",
             record_requirement=True, mandatory=True, allowed_channels=["IN_APP", "EMAIL"])
publish(reg["intent_id"], "IN_APP", "Consultation notice", "<p>Consultation notice {{invoice_no}}.</p>")
s, c, _ = req("POST", "/v1/communications", body={"intent_id": reg["intent_id"], "legal_entity_id": LE, "recipient_principal_id": "emp",
    "locale": "en-GB", "variables": {"invoice_no": "HR-1"}, "source_event_id": "evt-" + uuid.uuid4().hex[:8]})
rid = c["communication"]["communication_id"]
req("POST", f"/v1/communications/{rid}/prepare")
s, b, _ = req("POST", f"/v1/communications/{rid}/dispatch")
check("a regulated communication without its notice package → NCD-018", s == 422 and b.get("reason_code") == "NCD-018", (s, b))
req("POST", f"/v1/communications/{rid}/prepare")
deadline = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 3 * 86400))
s, n, _ = req("POST", "/v1/regulated-notices", body={"communication_id": rid, "legal_basis_ref": "PDC-UK-CONSULT-7",
    "recipient_capacity": "employee", "deadline_at": deadline, "wfc_obligation_ref": "wfc-1"})
check("POST /v1/regulated-notices builds the package; legal sufficiency NOT_DETERMINED_BY_NCD",
      s == 201 and n.get("legal_sufficiency") == "NOT_DETERMINED_BY_NCD" and n.get("content_hash"), (s, n))
req("POST", f"/v1/communications/{rid}/dispatch")
nv = wait_for(lambda: (lambda r: r[1] if r[1].get("notice", {}).get("state") == "ACK_PENDING" else None)(req("GET", f"/v1/regulated-notices/{n['notice_id']}", principal="emp")))
check("E3 notice delivered in-app only and awaits acknowledgment", nv and nv["notice"]["state"] == "ACK_PENDING" and nv.get("content"), nv and nv["notice"]["state"])
s, b, _ = req("POST", "/v1/acknowledgments", principal="mallory2", body={"notice_id": n["notice_id"], "notice_version": 1, "disposition": "ACKNOWLEDGED", "content_hash": n["content_hash"]})
check("only the authenticated recipient may acknowledge (NCD-017)", s == 422 and b.get("reason_code") == "NCD-017", (s, b))
s, b, _ = req("POST", "/v1/acknowledgments", principal="emp", body={"notice_id": n["notice_id"], "notice_version": 1, "disposition": "ACKNOWLEDGED", "content_hash": "00"})
check("NP-40 acknowledging content not shown → NCD-017", s == 422 and b.get("reason_code") == "NCD-017", (s, b))
s, ack, _ = req("POST", "/v1/acknowledgments", principal="emp", body={"notice_id": n["notice_id"], "notice_version": 1, "disposition": "ACKNOWLEDGED", "content_hash": n["content_hash"]})
check("POST /v1/acknowledgments binds actor, method and exact version", s == 201 and ack.get("actor_principal_id") == "emp" and ack.get("method") == "AUTHENTICATED_IN_APP", (s, ack))
s, rec, _ = req("POST", f"/v1/regulated-notices/{n['notice_id']}/record-declaration", body={"drc_record_ref": "drc://live/1@v1"})
check("DRC record declaration recorded", s == 200 and rec.get("record_status") == "DECLARED", (s, rec))

# ── outbox and legacy fixes ─────────────────────────────────────────────────
print("── canonical events and legacy fixes")
out, _ = sql(f"SELECT string_agg(DISTINCT event_type, ',' ORDER BY event_type) FROM event_outbox WHERE tenant_id='{TENANT}'")
need = {"communication.prepared", "communication.blocked", "delivery.attempt.created", "delivery.evidence.recorded",
        "endpoint.suppressed", "notice.acknowledged", "communication.record.declared"}
have = set(out.split(",")) if out else set()
check("§10.2 canonical events are in the transactional outbox", need <= have, sorted(need - have))
out, _ = sql(f"SELECT count(*) FROM event_outbox WHERE tenant_id='{TENANT}' AND published_at IS NULL")
check("the relay drains the outbox", wait_for(lambda: sql(f"SELECT count(*)=0 FROM event_outbox WHERE tenant_id='{TENANT}' AND published_at IS NULL")[0] == "t"), out)

s, sent, _ = req("POST", "/v1/notifications", principal="alice", body={"recipient_principal_id": "held", "legal_entity_id": LE,
    "channel": "EMAIL", "subject": "Hello", "body": "<p>hi</p>", "correlation_id": "corr-" + uuid.uuid4().hex, "recipient_address": "held@example.test"})
check("legacy POST /v1/notifications now honours canonical suppression (NCD-010)", s == 201 and sent.get("status") == "FAILED" and "NCD-010" in sent.get("failure_reason", ""), (s, sent))
s, sent, _ = req("POST", "/v1/notifications", principal="alice", body={"recipient_principal_id": "pat", "legal_entity_id": LE,
    "channel": "EMAIL", "subject": "Hello", "body": "<p>hi</p>", "correlation_id": "corr-" + uuid.uuid4().hex})
check("legacy register exposes PROVIDER_ACCEPTED, never SENT (§3.3)", sent.get("status") == "PROVIDER_ACCEPTED" and sent.get("stored_status") == "SENT", (s, sent.get("status")))
r = urllib.request.Request(BASE + "/v1/notifications/webhooks/smtp", data=b'{"event_id":"x","event_type":"BOUNCED","recipient_email":"victim@example.test"}', method="POST")
try:
    urllib.request.urlopen(r, timeout=10); code = 200
except urllib.error.HTTPError as e:
    code = e.code
check("legacy provider webhook refuses an unsigned request (INV-27)", code == 401, code)

print(f"\n{passes} passed, {failures} failed")
sys.exit(failures)
