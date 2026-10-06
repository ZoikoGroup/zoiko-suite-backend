# notification-svc — Operational Runbook

The ten runbooks ZS-SVC-Y-001 §13.2 requires, written against the controls this
service actually has. The alerts in `deployments/prometheus-rules.yml` under the
`notification` group each name a section here.

Port **8133**. Database `notification`. Topic `zoiko.notification.events`.

| Alert | Section |
|---|---|
| NotificationExceptionsAging | 2 |
| NotificationProviderFailing | 4.1 |
| NotificationCallbacksStopped, NotificationCallbacksRejected | 4.2 |
| NotificationSenderAuthBroken | 4.3 |
| NotificationComplaintOrBounceSpike | 4.5 |
| NotificationMisdeliveryOpen | 4.6 |
| NotificationNoticesPastDeadline, NotificationRecordHandoffOverdue | 4.7 |
| NotificationUnknownAttemptsAging | 4.8 |
| NotificationQueueStalled, NotificationOutboxStalled | 4.1, 4.4 |

Sections 4.4, 4.9 and 4.10 have no alert of their own. A duplicate storm, a
template defect and a suppression defect are each reported by a person before
any metric can tell them from normal traffic.

---

## 1. What this service is, in one paragraph

Every person-directed message the platform sends goes through here. The
control plane (`internal/ncd`, the `/v1/communications` family) turns an
approved intent and template into a durable job. It submits one immutable
attempt at a time through a certified provider binding (SMTP or the in-app
inbox), and records what the evidence supports. It never records "sent".
Beside it, the legacy register (`/v1/notifications`) still serves the console
and existing callers. Both paths pass the same suppression gate. Nothing is
ever deleted: suppressions are lifted, not removed, and evidence tables refuse
DELETE at the database (migrations 000022, 000023). Most incident steps
therefore **stop** something. None of them **erase** anything.

---

## 2. First response

```bash
curl -s localhost:8133/readyz                         # dependency health, not just liveness
curl -s localhost:8133/metrics | grep -E '^notification_(ncd|sender|outbox)'
docker logs --tail 200 notification-svc 2>&1 | grep -E '"level":"(error|warn)"'
```

Every control below is an ordinary authenticated API call. Through the
gateway, it carries the usual envelope. Direct to the service on a
workstation, it needs these headers:

```bash
H=(-H "X-Tenant-Id: $TENANT" -H "X-Principal-Id: $ME" -H "X-Legal-Entity-Id: $ENTITY"
   -H "X-Correlation-ID: incident-$(date +%s)" -H "Idempotency-Key: $(uuidgen)"
   -H "Content-Type: application/json")
```

The open human work queue, per tenant, is always the first read:

```bash
curl -s "${H[@]}" localhost:8133/v1/exceptions              # open only; ?open=false for history
```

---

## 3. Signals

| Metric | Meaning |
|---|---|
| `notification_ncd_unknown_attempts`, `_unknown_oldest_age_seconds` | Attempts that may or may not have reached a provider. Nothing resends them (INV-13). |
| `notification_ncd_queued_jobs`, `_queued_oldest_age_seconds` | Jobs waiting to submit. Age is the backpressure signal (§6.5). |
| `notification_ncd_open_exceptions{kind}` | Unresolved exceptions by kind: the human queue. |
| `notification_ncd_open_exception_oldest_age_seconds` | Age of the oldest of those. |
| `notification_ncd_notices_past_deadline` | Regulated notices at deadline without a concluded disposition. |
| `notification_ncd_record_declarations_pending`, `_record_pending_oldest_age_seconds` | Notices whose DRC record declaration has not arrived. |
| `notification_ncd_attempts_total{channel,binding,state}` | Provider submissions by outcome. |
| `notification_ncd_callbacks_total{binding,outcome}` | Provider callbacks: `applied`, `duplicate`, or `rejected_<code>`. |
| `notification_sender_auth_healthy` | 1 while DKIM/DMARC/SPF authenticate our mail, 0 while email is held. Present only where this service signs. |
| `notification_outbox_pending`, `_oldest_age_seconds` | Events committed but not yet published. |

Gauges are re-read from the database every 15 s under platform scope. They are
correct after a restart, and they carry no tenant, address or id (§13.3).
Which tenant is affected comes from `GET /v1/exceptions` and the logs, never
from a metric label.

---

## 4. Runbooks

### 4.1 Provider outage

**Automatic:** after 5 consecutive submission failures, a binding's circuit
opens (`health = CIRCUIT_OPEN`). Its jobs stay queued. Failover goes only to
another certified binding with the same channel, evidence class and
residency, so an outage defers mail and never reroutes it somewhere weaker
(NP-28). After 5 minutes a system-opened circuit goes `HALF_OPEN` and one
success closes it.

1. Confirm which binding: `GET /v1/provider-bindings` (look at `health`, `reason`).
2. If the provider has declared an incident, hold the circuit open yourself so
   half-open probes stop. An operator-opened circuit never auto-recovers:
   ```bash
   curl -s "${H[@]}" -X POST localhost:8133/v1/provider-bindings/smtp-primary/circuit \
        -d '{"open": true, "reason": "provider incident INC-123"}'
   ```
   Needs `NOTIFICATION_SUPPRESS` at platform scope.
3. Watch `notification_ncd_queued_oldest_age_seconds`. Jobs past `expires_at`
   conclude as `DELIVERY_EXPIRED` exceptions, never as silent drops. Raise a
   platform incident before urgent or regulated jobs reach that point.
4. **Restore:** the same call with `"open": false`. Queued jobs drain on the
   next worker pass. Then reconcile: check that `_unknown_attempts` returns to
   its baseline, and resolve any UNKNOWN left by the outage (4.8).

### 4.2 Webhook / callback outage

Jobs do not wait on callbacks. An accepted attempt waits 15 minutes for
evidence. With no negative fact it concludes at what the evidence supports,
usually `PROVIDER_ACCEPTED`. **A missing callback never marks a delivery
failed**, as §13.2 requires. A silent callback outage therefore makes delivery
claims weaker, not wrong, and only the callback rate shows it.

1. `NotificationCallbacksStopped`: callbacks were arriving and stopped while
   attempts continue. Check the provider's webhook dashboard and the gateway
   route `/v1/provider-events/{binding}`.
2. `NotificationCallbacksRejected`: callbacks are arriving and failing
   authentication. The `outcome` label carries the code. A signature failure
   almost always means the secret was rotated on one side only:
   `NCD_CALLBACK_SECRET_<BINDING>` for the plane, or
   `NOTIFICATION_WEBHOOK_SECRET[_<PROVIDER>]` for the legacy webhook. Both fail
   closed: no secret means every callback is refused (INV-27).
3. Reconcile from the provider's own event log once callbacks resume. Replays
   are deduplicated per tenant and binding, and a late bounce still suppresses
   the address. Never mark attempts failed by hand to "clean up".

### 4.3 Sender-domain authentication failure

Applies only where this service signs (`NOTIFICATION_DKIM_*` set). The monitor
re-reads the DKIM selector, DMARC and SPF every
`NOTIFICATION_SENDER_AUTH_CHECK_INTERVAL` (default 10 min). On a definite
break it **holds all email as retryable before the relay**: attempts go to
`RETRY_SCHEDULED` with the NP-55 reason and nothing reaches the provider. A
resolver timeout changes nothing in either direction.

1. `docker logs notification-svc | grep NP-55` names the problem (missing
   selector key, key mismatch, DMARC/SPF absent).
2. Fix DNS or the key, then **assess spoofing**: check DMARC aggregate reports
   for mail that used our domain while authentication was broken.
3. If the private key may be exposed, rotate it: publish the new selector
   first, then change `NOTIFICATION_DKIM_SELECTOR` and `_PRIVATE_KEY` together
   from the secret store and restart.
4. Reactivation is automatic on the next healthy check. The gauge returns to 1
   and held mail retries within its budget. Mail that outlived `expires_at`
   concludes as an exception and needs a governed resend (4.4, step 4).

### 4.4 Mass duplicate risk

The structural defences: a source event maps to one communication
(purpose-scoped idempotency, `NCD-019`), one attempt is in flight per
communication, and the database refuses a new attempt while one is UNKNOWN
(INV-13). A duplicate storm therefore means a caller is minting **new** source
events, or someone is resending.

1. Stop the source, narrowest first:
   - one intent: `POST /v1/communication-intents/{id}/retire` (no new communications);
   - one tenant's stream: `POST /v1/streams/{STREAM}` with `{"state":"PAUSED","reason":"…"}`;
   - one provider: open its circuit (4.1).
   `CRITICAL` streams cannot be paused (NP-53). For a critical-stream storm, open the circuit.
2. Find the defect: group `ncd_communications` by `source_event_id` and caller
   for the window. Different source ids for one business event is the caller's
   replay bug, not ours.
3. Resolve every UNKNOWN attempt before any resend (4.8).
4. Resend only through `POST /v1/communications/{id}/resend` with a reason. It
   is governed and refused while an attempt is UNKNOWN (`NCD-014`).

### 4.5 Complaint spike

**Automatic:** each worker pass evaluates 24 h reputation per tenant, stream
and binding, over at least 20 attempts. More than 0.3 % complaints on
MARKETING, or more than 5 % hard bounces on any stream, pauses that stream
(never CRITICAL) and raises `REPUTATION_ALERT`. Complainants and hard bounces
are already suppressed by the callback that reported them.

1. `GET /v1/reputation` shows the rates and stream states for the tenant.
2. Investigate audience, consent provenance, template and sender. Read the
   intents and templates that sent in the window.
3. Resume only with a stated reason: `POST /v1/streams/MARKETING` with
   `{"state":"ACTIVE","reason":"…"}`.

### 4.6 Sensitive-data misdelivery

```bash
curl -s "${H[@]}" -X POST localhost:8133/v1/communications/$COMM/misdelivery \
     -d '{"reason": "sent to former employee address, INC-456"}'
```

This cancels queued and awaiting-evidence jobs for the communication, puts the
endpoint on a `SECURITY_HOLD` suppression for all channels and purposes, and
raises `MISDELIVERY_INCIDENT`. **It deletes nothing.** The evidence of what was
sent where is preserved, and the database would refuse a DELETE anyway
(000023).

1. Open the security/privacy incident workflow with the communication id. The
   evidence bundle is at `GET /v1/evidence/{communication_id}`.
2. Lifting the hold needs evidence and a second principal:
   `POST /v1/suppressions/{id}/lift` creates an approval for someone else to decide.

### 4.7 Regulated notice deadline risk

The worker raises `DEADLINE_AT_RISK` inside the at-risk window and
`ACK_EXPIRED` at the deadline. It never fabricates an acknowledgment. Legal
sufficiency is always `NOT_DETERMINED_BY_NCD` (§8.5).

1. `GET /v1/regulated-notices/{id}` shows the routes tried and the evidence held.
2. Escalate to the owning workflow with that exact state. A manual service
   method (courier, hand delivery) is recorded as evidence, maker-checker:
   `POST /v1/regulated-notices/{id}/manual-evidence` with
   `{"kind":…, "artifact_ref":…, "reason":…}` creates an approval for a second principal.
3. `NotificationRecordHandoffOverdue`: a concluded notice has no DRC record
   declaration. Declare it once DRC has the package:
   `POST /v1/regulated-notices/{id}/record-declaration` with `{"drc_record_ref":…}`.

### 4.8 Stuck UNKNOWN attempts

An attempt is UNKNOWN when we cannot tell whether the provider took it: a
timeout after submission, or a process lost mid-submit. It must be reconciled,
never blindly resent. Past its 30-minute deadline it raises `UNKNOWN_UNRESOLVED`.

1. Query the provider by the attempt's `idempotency_token` (sent as
   `X-Zoiko-Idempotency-Token`) or by its provider message id. For SMTP, check
   the relay's logs for that token.
2. Record what you found, with the evidence reference:
   ```bash
   curl -s "${H[@]}" -X POST \
        localhost:8133/v1/communications/$COMM/attempts/$ATTEMPT/resolve \
        -d '{"resolved_state": "FAILED", "evidence_ref": "relay-log-2026-10-06#L812", "note": "relay has no record"}'
   ```
   `ACCEPTED` or `DELIVERED` if the provider has it, `FAILED` if it provably
   does not. FAILED permits the governed fallback. Needs
   `NOTIFICATION_RESOLVE_OUTCOME`. Legacy register rows use
   `POST /v1/notifications/{id}/resolve-delivery-outcome`.

### 4.9 Template defect

Published template versions are immutable (a database trigger), so a defect is
corrected by a new version, never an edit.

1. Stop new sends: `POST /v1/templates/{id}/retire`. A retired version never
   dispatches. Communications already pinned to it keep their render as
   evidence of what was actually sent.
2. Author the corrected version, then validate, approve (a different
   principal) and publish it: `/v1/templates/{id}/{validate|approve|publish}`.
3. Whether recipients need a correction notice is the owning domain's
   decision (with PDC where it applies), sent as a new communication.

### 4.10 Suppression propagation defect

Every send path, legacy or plane, reads both the canonical and the legacy
suppression lists immediately before the provider, and fails closed if they
cannot be read (NP-56). A propagation defect is therefore most likely an
address that was never suppressed, not a suppression that was ignored.

1. Suppress the affected endpoints now, with the source evidence:
   `POST /v1/suppressions` with `channel`, `address`, `channel_scope`,
   `purpose_scope`, `reason`, `source`, `source_evidence_ref`.
2. **There is no platform-wide emergency purpose/channel switch.** The nearest
   control is pausing the stream per tenant (`POST /v1/streams/{STREAM}`), or
   opening the binding's circuit for every tenant at once (4.1). Use the
   circuit for anything urgent.
3. Reconcile the provider's own suppression and bounce lists against
   `GET /v1/suppressions`, and notify the privacy/compliance owner.

---

## 5. Known limits worth knowing during an incident

- **`MISSING_CALLBACK` is never raised.** The kind exists in the schema, but a
  missing callback concludes at `PROVIDER_ACCEPTED` instead (4.2). The callback
  rate alert is the only signal.
- **No SMTP status query.** Reconciling an UNKNOWN SMTP attempt (4.8) means
  reading the relay's logs. There is no provider API to call.
- **Stream pause is per tenant**, and CRITICAL cannot be paused. The circuit
  breaker is the only control that stops a provider for everyone.
- **The four operator actions** (`TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`,
  `NOTIFICATION_SUPPRESS`, `NOTIFICATION_RESOLVE_OUTCOME`) are in the dev RBAC
  seed only. Check that the on-call role holds them before an incident needs them.
