# BIZ-10 Notification — Certification Status

**Service host:** notification-svc (its own service; it also hosts BIZ-03 templates, see `BIZ03_CERTIFICATION.md`)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 12
**Status: NOT CERTIFIED.** This is the most heavily built and tested service in the BIZ set: it carries its own 110-row certification matrix against ZS-SVC-Y-001 (`CERTIFICATION_MATRIX.md`, 100 met, 7 partial, 3 not met, the ten open rows blocked on other components). This audit checked the BIZ-10 rows against the code, did not re-derive that matrix, and found one class of defect: authorization checks that were skipped instead of refused. The spec's event names, event delivery and live-stack checks remain open.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 12)

| Spec row | State | Where |
|---|---|---|
| Commands: QueueNotification, Render, Send, Retry, Suppress, Cancel, ResolveDeliveryOutcome | All present, by inspection of the routes and handlers. Queue: `POST /v1/notifications`, `POST /v1/communications`, `POST /v1/notifications/events/ingest`. Render: `POST /v1/render-previews`, `/v1/communications/{id}/prepare`. Send: `/v1/communications/{id}/dispatch`. Retry: `/resend` and the retry worker. Suppress: `/v1/suppressions`, `/v1/notifications/suppression`. Cancel: `/cancel`. Resolve: `/resolve-delivery-outcome`, `/v1/communications/{id}/attempts/{attempt_id}/resolve` | `internal/handler`, `openapi.yaml` |
| Queries: GetNotification, GetAttempt, GetDeliveryStatus, ListFailures, GetRenderedMessage | GetNotification, GetDeliveryStatus present. **GetAttempt is a list** (`GET /v1/notifications/{id}/attempts`); there is no single-attempt read. **ListFailures is the general list with a `status` filter** (`GET /v1/notifications?status=FAILED`), not a dedicated query. The rendered subject, body and hash are returned by GetNotification | handler |
| Lifecycle Queued -> Sending -> Sent -> Delivered/Failed/Pending-Unknown/Suppressed/Cancelled | Implemented with the NCD precision: SENT means a provider accepted it and is never labelled delivered (ZS-SVC-Y-001 section 3.3, matrix NP-35 and INV-11). Delivered is only claimed for in-app and where delivery evidence exists. The legacy `status` and the precise delivery state are both exposed | `internal/domain`, matrix |
| Authorization / SoD | `NOTIFICATION_SEND`, `NOTIFICATION_VIEW`, `NOTIFICATION_RESOLVE_OUTCOME` (apart from send), `NOTIFICATION_SUPPRESS`; recipient preference and mandatory-notice policy are evaluated separately from sending (NCD plane) | handler, matrix |
| Events (5) | **Names differ from the spec and are not on the shared eventing outbox.** The service emits `notification.sent`, `notification.failed`, `notification.outcome_unknown`, `communication.prepared`, `communication.cancelled`, `delivery.attempt.created`, `delivery.attempt.unknown`, `delivery.evidence.recorded` and the `notice.*` set to its own `event_outbox`, in the same transaction as the change. **There is no `NotificationQueued` (the nearest is `communication.prepared`) and no `NotificationDelivered` (the nearest is `delivery.evidence.recorded`, which carries the evidence strength).** None has a catalogue entry or a published schema. See open item 1 | `internal/events`, `internal/store/outbox.go` |
| Failure: provider timeout becomes Pending/Unknown; retries keep subject and idempotency; no duplicate mandatory notice | Implemented and tested in the matrix: stranded SUBMITTING attempts become UNKNOWN, resends pin the original render, idempotency keys are persistent | matrix NP-57 and others |
| Evidence: trigger event, recipient/channel, template/version, rendered hash, attempt/reference, timestamps, delivery evidence | Present: pinned intent, template version, locale, content hash and endpoint on each attempt (immutable by trigger), attempt chain, append-only delivery evidence | matrix INV-04 |

## Defects found and fixed in this audit

Four authorization checks were skipped when something was unset, instead of refusing:

1. **`GET /v1/notifications/intents/{id}` served an intent with no legal entity to anyone in the tenant.** The check ran only when the intent had a legal entity, so an intent without one (the column is `NOT NULL` but accepts an empty string) was returned with its recipient address and render to any principal, with no authorization. It is now refused with 403 and carries no intent data.
2. **The same endpoint skipped the check entirely when no authorizer was configured.**
3. **`POST /v1/notifications/events/ingest` skipped authorization when no authorizer was configured.**
4. **Both suppression admin endpoints (`GET` and `POST /v1/notifications/suppression`) skipped it likewise.**

In production the authorizer is always set at startup, so 2 to 4 are latent; 1 needs only an intent with an empty legal entity. All now fail closed: an unset authorizer answers 503 `authorization not configured`.

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | The service owns intents, renders, attempts, provider references and outcomes, not the underlying obligation: it carries no obligation state, by inspection and the matrix (INV-29: a read-only projection cannot write authoritative tables) | Covered |
| Tenant isolation | `TestLegacyTables_RLSIsolatesTenantsAsTheAppRole` (every legacy tenant table, app role without superuser or RLS bypass) and `TestNCD03_TenantIsolation`; `TestHandler_GetIntent_TenantIsolation` at the handler. The shared `eventing_outbox` table is excluded from the RLS test by design (see `BIZ03_CERTIFICATION.md`) | Covered |
| Classification / retention | Sensitive-content rules are enforced on templates and sends (matrix TC-18), and the record-vault and housekeeping workers exist; per-notification retention classification was not checked in this audit | Partial |
| Negative paths | The matrix lists NP-01 to NP-60 with named tests; this audit added fail-closed authorization tests | Covered, per the matrix |
| Evidence lineage | Pinned render, template version, content hash, attempt chain and delivery evidence are immutable or append-only | Covered, per the matrix |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           handler package passes (stub-based, no database needed);
                                 the Postgres-backed store suite was not re-run for this change
                                 (no store file changed) and passed earlier the same day against
                                 postgres:16-alpine after the BIZ-03 event migration
new tests (internal/handler/biz10_test.go)
  TestGetIntent_NoLegalEntity_IsRefusedNotServed        fails with the new check removed (served 200)
  TestSuppressionAdmin_UnsetAuthorizerFailsClosed       fails with the new check removed (nil dereference)
```

Date: 2026-11-17.

## NOT verified

- The matrix's own claims were not re-run or re-derived beyond the above, and its ten open rows are unchanged.
- Whether the `/v1/notifications/events/ingest` endpoint's missing-authorizer branch is exercised: the handler returns "orchestrator not configured" first when there is none, so only the suppression and intent endpoints have a test for the unset authorizer.
- No Docker image build or container run of this change, and the docker-compose stack was not run.
- Real authorization-svc decisions for the `NOTIFICATION_*` actions (tests use stubs).
- Real providers, real Kafka delivery, and the ORG contact and INT provider adapter dependencies.
- Whether any caller depends on the old behaviour of the intent read for an intent with no legal entity (none was found in this repository).

## Open items to reach certification

1. **Events:** move the notification events to the shared eventing outbox with canonical names, schemas and catalogue entries, as done for BIZ-01, 02, 03, 06, 08 and 09, and decide the mapping to the spec's `NotificationQueued`, `NotificationSent`, `NotificationDelivered`, `NotificationFailed` and `NotificationOutcomeUnknown`. This is the largest remaining gap: the service has about twenty event types on its own outbox, with a database `CHECK` listing them, so it needs a mapping decision rather than a mechanical move. In particular `NotificationDelivered` conflicts with the deliberate rule that a provider acceptance is never called delivery.
2. Add a single-attempt read and a dedicated failures query if the spec names are meant literally.
3. Per-notification retention and classification treatment (the audit did not verify it).
4. Close the ten matrix rows blocked on other components (estate adoption, PRV, DRC, WFC, XIC, MDM/IAM) and decision OD-05.
5. Register and verify the `NOTIFICATION_*` and `TEMPLATE_*` actions in authorization-svc; run the service in the compose stack.
6. Make the ingest endpoint's authorizer check testable (the orchestrator guard comes first).
