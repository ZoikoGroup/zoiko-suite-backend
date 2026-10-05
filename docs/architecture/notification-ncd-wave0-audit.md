# ZS-SVC-Y-001 Wave 0: audit of `notification-svc` against the NCD specification

Date: 2026-10-05. Method: read-only. Source: `ZS-SVC-Y-001_Notification_Communication_Delivery_Control_Detailed_Service_Specifications_v1.0.docx`
against `services/notification-svc` (14 migrations, ~14.3k lines of non-test Go, 347 test functions): migrations, domain types, handlers, stores, deliverers,
webhook, ledger, policy and event publisher were read; selected code paths were searched. **Nothing was executed**: no test run, no database, no provider.
Where a claim rests on absence ("no code does X"), it rests on a search of the service and is marked **(search)**; a claim that needs a run is marked **UNVERIFIED**.
No code was changed by this wave.

## 1. What `notification-svc` is today

It already is the NCD control plane in embryo (its code cites Y-001 sections 1.4, 3.4, 6.2, 10.2). It contains **two send paths that do not share an identity**:

| | Direct path | Ledger pipeline |
|---|---|---|
| Entry | `POST /v1/notifications` | `POST /v1/notifications/events/ingest` |
| Governing spec (per code comments) | Y-001 | ZS-COMMS-EMAIL-001 v2.0 |
| Records | `notifications`, `notification_delivery_attempts` | `message_intents`, `message_renders`, `delivery_attempts`, `delivery_events` |
| Content | static embedded catalogue, or governed BIZ-03 template, or free text | ledger catalogue of Go templates (seed `ZS-IA-001` etc.) |
| Policy | none found **(search)** | 8-level precedence engine with suppression, kill switch |
| Delivery | shared `Deliverer` (SMTP, secondary failover) | the same `Deliverer`, but through a transient `Notification` value |
| Retry / unknown | retry worker, stranded sweep, `PENDING_UNKNOWN`, submission marker, reasoned resend | not found **(search)** |

Shared: tenant isolation (forced RLS on every table that migrations create), the event outbox, the webhook ingest and suppression list.

## 2. Findings (ordered by severity)

| ID | Sev | Finding | Spec reference |
|---|---|---|---|
| F-01 | **High, FIXED in Wave 1** | Provider callbacks are accepted **without authentication**. `/v1/notifications/webhooks/*` is exempt from the request envelope (`cmd/server/main.go:314`), and the webhook package contains no signature or HMAC check **(search)**. HMAC exists only for action-link tokens. State changes (delivery, bounce, complaint, suppression) can therefore be caused by anyone who can reach the route. | INV-27, NP-26, TC-12 |
| F-02 | **High** | Two send paths with no shared `communication_id`. The ledger path writes no `notifications` row; the direct path writes no `message_intents`. Controls exist on one path or the other, never both. | INV-02, Section 9.1 |
| F-03 | **High, FIXED in Wave 1** | The direct path performs **no suppression, permission or kill-switch check** before submit **(search)**. Hard-bounced or opted-out recipients are reachable through it. | INV-24, NP-13, NP-14 |
| F-04 | Med | Three template sources: embedded static catalogue, governed BIZ-03 (`template_versions`), ledger Go catalogue. Only BIZ-03 has the publication lifecycle. | NCD-01, INV-03 |
| F-05 | Med | On the governed-template path the **subject is caller free text**; templates carry no subject. Sensitive facts can reach a subject. CRLF is stripped at the SMTP layer (`sanitizeHeaderValue`), not rejected and audited. | INV-17, NP-08, NP-32 |
| F-06 | Med | `recipient_address` free text is accepted with provenance `REQUEST`. There is no regulated-notice restriction. | INV-15, NP-11 |
| F-07 | Med | Extra, undeclared variables are silently ignored; `variable_schema` is `[]string` (names only, no types, sensitivity or escape policy). Missing required variables are refused (good). | NP-07, Section 4.2 |
| F-08 | Med | BIZ-03 template state **edges are not enforced in the database**: the trigger freezes content and terminal states and a CHECK enforces maker-checker (`approved_by <> created_by`), but nothing stops a direct SQL `DRAFT` to `PUBLISHED`. | TC-02 |
| F-09 | Low | `RenderPreview` renders any version (including DRAFT) with caller-supplied variables; the spec requires synthetic or authorized data only. | Section 4.5 |
| F-10 | Low, PARTLY FIXED (direct attempts) | `provider_message_id` has a **non-unique** index (migration 000009). A message id mapping to two attempts is not prevented. | NP-27, Section 7.5 |
| F-11 | Low | `progress.md` is dated 2026-09-08 and omits migrations 000007-000014. Known-gaps rows 97c (no caller) and 97d (unbounded register) are still open. | n/a |

## 3. Section-by-section checklist

Status key: **BUILT** (verified in code), **PARTIAL**, **GAP** (not built), **UNVERIFIED** (needs a run).

### NCD-01 Intent, template and content registry
| Item | Status | Evidence |
|---|---|---|
| Template definitions and versions, locales, content hash | BUILT | `template_definitions`, `template_versions` (000005) |
| Lifecycle DRAFT, REVIEW, APPROVED, PUBLISHED, RETIRED/SUPERSEDED | BUILT | `domain/template.go`, `ValidateTemplate`/`Approve`/`Publish` |
| Published content immutable | BUILT | trigger `reject_template_version_mutation` |
| Maker-checker on approval | BUILT | CHECK + store check (`CreatedBy == ApprovedBy`) |
| State edges enforced in DB | GAP | F-08 |
| First-class communication intent (purpose class, sensitivity, evidence class, channels, `marketing_allowed`, versioned) | GAP | no `intent_id` entity; `message_intents` is a per-event record |
| Typed variable contract (type, authority, sensitivity, escape) | GAP | `variable_schema []string` (F-07) |
| Reject unknown variables | GAP | F-07 |
| Attachment slots bound to DRC | GAP | no attachment code **(search)** |
| Subject from safe typed variables only | GAP | F-05 |
| HTML sanitization at publish | PARTIAL | `html/template` parse and contextual escaping at render; no allowlist or URL-slot policy |
| Locale: no silent fallback | PARTIAL | governed path requires an explicit locale and a per-locale published version; no regulated-content classification |
| Effective-dated publish, `GET /intents/{id}/effective` | GAP | `published_at` only; no knowledge-time lookup |

### NCD-02 Recipient, channel, preference, suppression
| Item | Status | Evidence |
|---|---|---|
| Endpoint resolved from IAM with provenance | BUILT | `internal/identity`, `RecipientAddressSource` |
| Suppression store (hard bounce, complaint, unsubscribe) | BUILT | `email_suppressions` (000008) |
| Precedence engine | PARTIAL | ledger path only; tests show S0/T0 bypass unsubscribe, S0 blocked by hard bounce, M1 blocked by unsubscribe, unknown class fails closed |
| Suppression checked atomically before submit | PARTIAL | ledger checks before render; direct path none (F-03); gap between check and submit **UNVERIFIED** |
| PDC applicability / PRV permission | GAP | no calls to either **(search)** |
| Preference profile, `/preferences` | GAP | none |
| Quiet hours, recipient time zone (INV-23) | GAP | none |
| Channel decision / recipient plan / `/revalidate` | GAP | none |
| Cross-tenant recipient guard | PARTIAL | RLS isolates tenants; no external-party relationship concept |
| Free-text endpoint control for regulated | GAP | F-06 |

### NCD-03 Delivery orchestration, routing, attempts
| Item | Status | Evidence |
|---|---|---|
| Durable per-attempt record | BUILT | `notification_delivery_attempts` (000011) |
| UNKNOWN state, no blind resend | BUILT | `PENDING_UNKNOWN`, `ResolveDeliveryOutcome`, submission marker (000013) |
| Purpose-scoped idempotency | BUILT | 000012 (optional by ruling 2026-09-29) |
| Reasoned resend preserving the chain | BUILT | 000014, `resend_handler.go` |
| Retry with backoff, stranded sweep | BUILT | `internal/retry` |
| Secondary-provider failover | PARTIAL | SMTP primary/secondary; "pre-certified equivalent" not modelled |
| `communication_id` / job / attempt split | GAP | F-02; `communication_id` appears in events as the notification id |
| Provider bindings (XIC), tenant and region | GAP | one configured SMTP set; no binding registry |
| `not_before`, `expires_at`, cancel | GAP | none |
| Governed multi-channel fallback keeping evidence class | GAP | none |
| Rate, abuse, storm controls; bulk preview | GAP | no rate limiting found **(search)** |
| Provider idempotency token | GAP | none found |

### NCD-04 Evidence, bounce, complaint
| Item | Status | Evidence |
|---|---|---|
| Webhook ingest, normalizer, DLQ with reprocess worker | BUILT | `internal/webhook`, 000009 |
| Duplicate callback idempotent | BUILT | `TestWebhook_DuplicateEvent_Idempotent` |
| Hard bounce, complaint, unsubscribe create suppression | BUILT | webhook tests; soft bounce does not suppress |
| Callback authentication | BUILT (Wave 1) | per-provider HMAC, replay window, fail-closed |
| Provider message id to one attempt | GAP | F-10 |
| Normalized evidence vocabulary and strength | PARTIAL | statuses recorded; spec vocabulary (`MAILBOX_ACCEPTED`, `OPEN_SIGNAL` ...) and confidence not modelled |
| Callback before API response, late corrections | UNVERIFIED | |
| Reconciliation job; reputation metrics; circuit breakers | GAP | metrics exist; no reconciliation or breaker found **(search)** |
| DKIM / SPF / DMARC posture monitoring | GAP | none; operational |
| RFC 8058 one-click unsubscribe | BUILT | `/v1/notifications/unsubscribe`, `List-Unsubscribe` header (ledger path) |
| Open and click tracking | N/A | none exists (default-off is satisfied) |

### NCD-05 Regulated notice and acknowledgment
| Item | Status |
|---|---|
| Entire service (notice package, lifecycle, acknowledgment, correction and supersession, WFC and DRC handoff) | GAP. Only `read_at` for in-app notices exists, which is a read marker and not an acknowledgment |

### Cross-cutting
| Item | Status | Evidence |
|---|---|---|
| Tenant isolation, forced RLS | BUILT | all tables from 000002 onward |
| Authorization actions | BUILT | `NOTIFICATION_SEND`, `_VIEW`, `_RESOLVE_OUTCOME`, `_SUPPRESS` |
| Event outbox | BUILT | `event_outbox` (000010) |
| Events from spec 10.2 | PARTIAL | `delivery.attempt.created`, `delivery.attempt.unknown` exist; `communication.*`, `endpoint.suppressed`, `notice.*` do not |
| Stable error codes NCD-001..020 | GAP | service has its own codes |
| Channels | PARTIAL | EMAIL and IN_APP work; SMS withdrawn; WEBHOOK refused (correctly out of scope); push absent |
| Declared record handoff to DRC; WFC escalation | GAP | none |
| Callers | GAP | **no other service sends a notification** (known-gaps 97c) |

## 4. NP-01..NP-60 negative-path matrix

Basis: code and test names read; **not run**. Counts are at the end.

| NP | Status | Note |
|---|---|---|
| 01 Domain calls provider SDK directly | GAP | no architecture test or secret policy |
| 02 Unknown intent id | PARTIAL | unknown template fails closed; no intents |
| 03 Retired / non-effective intent | PARTIAL | only published template versions are used |
| 04 Draft template referenced | BUILT | governed path uses `GetPublishedVersion` |
| 05 Published template edited in place | BUILT | DB trigger |
| 06 Required variable missing | BUILT | refused on static, governed and ledger paths |
| 07 Unexpected variable | GAP | F-07 |
| 08 CRLF in subject or address | PARTIAL | stripped, not rejected or audited |
| 09 Unapproved locale, regulated | PARTIAL | no silent fallback; no regulated class |
| 10 Attachment "latest" pointer | GAP | no attachments |
| 11 Free-text legal recipient | GAP | F-06 |
| 12 Recipient in another tenant | PARTIAL | RLS only |
| 13 Endpoint previously hard-bounced | BUILT (Wave 1) | both paths; but see F-12: bounces for direct sends are not matched |
| 14 Marketing opt-out | PARTIAL | ledger yes; direct path is treated as transactional so opt-out does not block it |
| 15 Muted SMS, urgent security policy | GAP | no preferences |
| 16 Muted SMS, routine reminder | GAP | no preferences |
| 17 PRV INDETERMINATE | GAP | no PRV |
| 18 Mandatory notice vs privacy block | GAP | no PRV |
| 19 Quiet hours, routine | GAP | none |
| 20 Recipient time zone missing | GAP | none |
| 21 Duplicate source event | BUILT | correlation id, purpose key, ledger dedup key |
| 22 Timeout after submit | BUILT | `PENDING_UNKNOWN`, marker |
| 23 Provider 500 before known submit | PARTIAL | retry worker; no provider idempotency token |
| 24 Callback duplicated | BUILT | tested |
| 25 Callback before API response | UNVERIFIED | |
| 26 Callback signature invalid | BUILT (Wave 1) | HMAC verification, tested; was GAP |
| 27 Provider message id maps to two attempts | PARTIAL (Wave 1) | unique index on direct attempts (000015); ledger table still non-unique |
| 28 Primary provider outage | PARTIAL | secondary SMTP failover, tested |
| 29 Fallback lowers evidence class | GAP | no evidence class |
| 30 Fallback violates residency | GAP | no residency routing |
| 31 SMS exposes S3 payload | GAP | no sensitivity model |
| 32 Subject contains sensitive fact | GAP | F-05 |
| 33 Secure-link token forwarded | PARTIAL | signed action tokens, link-scanner safe; audience binding UNVERIFIED |
| 34 Attachment hash differs | GAP | no attachments |
| 35 Provider says accepted | PARTIAL | `SENT` means provider accepted (documented); spec wants distinct terms |
| 36 SMTP mailbox accepted | PARTIAL | not distinguished from provider accepted |
| 37 Open pixel never fires | BUILT | no tracking exists, nothing inferred |
| 38 Open pixel via prefetch | BUILT | no tracking exists |
| 39 Click without authentication | GAP | no acknowledgment model |
| 40 Acknowledge superseded version | GAP | NCD-05 |
| 41 Operator marks notice served | GAP | NCD-05 |
| 42 Acknowledgment deadline expires | GAP | NCD-05 |
| 43 Correction after original | GAP | NCD-05 |
| 44 Mistaken recipient | GAP | no incident path |
| 45 Complaint then provider switch | PARTIAL | suppression is keyed by tenant, email and stream, not provider; no migration test |
| 46 Unsubscribe callback delayed | BUILT | direct endpoint writes suppression |
| 47 Purchased list, no consent | GAP | no PRV |
| 48 Promo module in transactional template | GAP | no purpose class |
| 49 Bulk query spans tenants | PARTIAL | RLS; no bulk API |
| 50 Audience changes preview to send | GAP | no bulk |
| 51 Rate limit backlog | UNVERIFIED | no rate controls found |
| 52 Job expires before submit | GAP | no `expires_at` |
| 53 Security alert vs marketing blast | GAP | no priority classes on queue |
| 54 Template provider assets unavailable | BUILT | templates embedded locally |
| 55 DMARC/DKIM breaks | GAP | no monitoring |
| 56 Suppression table unavailable | BUILT (Wave 1) | both paths fail closed, tested |
| 57 Evidence write fails after delivery | PARTIAL | outcome recorded on a detached context; stranded sweep; outbox |
| 58 DRC declaration fails | GAP | no DRC |
| 59 Provider corrects status later | UNVERIFIED | |
| 60 Tenant disables unsubscribe | PARTIAL | no such setting exists; no explicit guard |

## 5. Dependency readiness

| Needs | Service | Ready? |
|---|---|---|
| IAM contact | `identity-context-svc` | Yes, already called |
| PRV decision | `privacy-decision-svc` (`POST /`) | Exists; contract not yet read against NCD needs |
| PDC rules | `jurisdiction-rules-svc`, `policy-svc` | Exists; communication rules (quiet hours, unsubscribe) not authored |
| WFC | `workflow-svc`, `exception-escalation-svc` | Exists |
| DRC | `document-vault-svc`, `retention-registry-svc` | Exists |
| XIC provider bindings | `secret-vault-integration-svc`, `key-management-svc` | Credentials only; **no binding registry** |
| MDM contact / time zone | `tenant-entity-registry-svc`, `org-structure-svc` | Time-zone source unconfirmed |

## 6. Proposed next steps (Wave 1 candidates)

1. Close **F-01** first: authenticate provider callbacks (per-provider signature check plus replay window), keep DLQ behaviour. Small, self-contained, highest risk.
2. Close **F-03**: put suppression and kill switch in front of the direct path, or decide it is retired in favour of the ledger path.
3. Decide the **single communication identity** (F-02) and the migration for existing rows.
4. Refresh `progress.md`; add NP-01..60 as a tracked test list, starting with the BUILT rows.
5. Decisions to close before Wave 2: evidence classes E0-E4 (OD-06), secure-link model vs existing action tokens (OD-05), email provider (OD-02), and which spec governs where Y-001 and ZS-COMMS-EMAIL-001 differ.

## 7. Tally

NP matrix, 60 scenarios: **10 BUILT, 17 PARTIAL, 30 GAP, 3 UNVERIFIED**. The audit does not claim any scenario is certified; BUILT means the behaviour is present in code and, where noted, tested.
The 30 gaps are concentrated in the parts that depend on services the NCD does not yet call (PRV, PDC, DRC, XIC bindings, MDM) and in NCD-05, which is not built.

## 8. Wave 1 progress

**F-01 fixed (callback authentication).** `internal/webhook/verify.go`; wired in `cmd/server/main.go`; config `NOTIFICATION_WEBHOOK_SECRETS` / `NOTIFICATION_WEBHOOK_TOLERANCE`.
* Scheme: `X-Zoiko-Signature: t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<body>">`, per-provider secrets (several per provider for rotation, 16-byte minimum), 5-minute tolerance, constant-time compare.
* Fail closed: a provider with no secret is refused; a handler with no verifier answers 503; every verification failure is the same 401 and is logged with its reason; a rejected callback changes no state and does not reach the DLQ. Bodies over 2 MB get 413 instead of being truncated.
* Config refuses a malformed or short secret list at start-up rather than silently closing the ingress.
* **Deployment impact:** until `NOTIFICATION_WEBHOOK_SECRETS` is set, every provider callback is refused (a startup warning says so). Delivery receipts, bounces and complaints will not be recorded until it is. Whoever sends the callbacks (provider gateway or XIC ingress) must sign with the same scheme.
* **Not done:** provider-native schemes (SNS certificate, SendGrid ECDSA) because no provider is selected (OD-02); the compose files do not yet set the variable.
* Tests: `verify_test.go` (accept, 13 refusal cases, timestamp-rebinding, rotation, short secrets, forged callbacks change no state, no verifier means closed, oversized body) and `config_test.go`; `TestWebhook_HTTPHandler_Routing` now signs its requests. Service tests pass. The Docker race run could not be made because the Docker daemon was not running when tried.

**Open for Wave 1, needs a decision:** F-02 (single communication identity) and F-03 (direct path skips suppression and kill switch).

**F-03 fixed (suppression and kill switch on the direct path).** `internal/policy/direct_guard.go`, wired in `cmd/server/main.go` around the `Deliverer` that `POST /v1/notifications`, the retry worker and a resend all share, so it applies immediately before the provider is called (INV-24).
* EMAIL only. A suppressed address is a terminal FAILED outcome and the provider is not called; an engaged kill switch is a retryable hold; a suppression lookup error fails closed (INV-30). The ledger pipeline keeps its own class-aware policy and the unwrapped deliverer.
* **Assumption:** a direct send has no communication class, so it is checked as TRANSACTIONAL / T0: hard bounces, complaints and administrative suppressions block it, an unsubscribe does not. It should give way to an explicit purpose class when NCD-01 lands.
* Tests: `policy/direct_guard_test.go` (6) and `handler/direct_guard_e2e_test.go` (suppressed recipient never mailed, clean recipient delivered, kill switch holds for retry).

**New finding F-12 (found while planning F-02):** webhook callbacks are matched only against the ledger `delivery_attempts`. A direct-path send leaves no `provider_message_id` a webhook can match, so a bounce or complaint for a direct send goes to the DLQ unresolved and never creates a suppression. Plan: `notification-ncd-communication-identity-plan.md`, step 1.

**F-02:** plan written, awaiting a decision (`notification-ncd-communication-identity-plan.md`).

**F-12 fixed (callbacks now match direct sends).** Migration `000015` adds `notification_delivery_attempts.provider_message_id` (nullable, unique where set); `domain.ExtractProviderMessageID` takes the id out of the transport receipt when an attempt is ACCEPTED; `LookupAttemptByProviderMessageID` now searches the ledger and then the direct attempts, and accepts the id with or without angle brackets.
* Effect, proven against a real Postgres 16: a hard-bounce callback quoting a direct send's Message-ID creates the suppression, and the F-03 guard then refuses that address (`TestDirectAttempt_HardBounceCallbackBecomesASuppression`).
* **Also fixed (F-13, found on the way):** the ledger path stored the transport's whole receipt sentence as `provider_message_id`, so a real callback quoting the Message-ID never equalled it. It now stores the extracted id (a bare token with no spaces is kept as is); the receipt is still returned to the caller as `provider_response`.
* **Not backfilled:** rows written before 000015 keep a NULL id and remain unmatchable. Deriving ids from old free-text receipts would be a guess presented as evidence.
* **Not done:** a callback for a direct send creates the suppression but is not stored as a delivery event. The `delivery_events` table is keyed to ledger attempts and intents; evidence for direct attempts belongs to the NCD-04 evidence work (Wave 5) or to the identity unification (plan step 4).
* Tests added: 4 store tests plus a migration down/up test (real Postgres), id-extraction unit tests. Full service suite passes with `TEST_DATABASE_URL` set (a throwaway embedded Postgres on another port, not the instance already running on 5432).

**F-02 step 2 done (the link between the two identities).** Migration `000016` adds `message_intents.notification_id` and `notifications.message_intent_id` (nullable, one-to-one by partial unique indexes, `ON DELETE SET NULL` so housekeeping purging an intent never blocks or harms the notification). A trigger enforces: same tenant (under the caller's row-level security, so an invisible row fails closed), a set link cannot be repointed, and the two sides must agree (a row cannot claim a counterpart that already points elsewhere). `PgStore.LinkIntentToNotification` sets both sides in one transaction, idempotently, and tells "not found" from "already linked elsewhere"; `NotificationIDForIntent` / `IntentIDForNotification` read either end.
* **No behaviour change:** nothing calls the link yet. Step 3 (the ledger path creates the `notifications` row and links it, behind a flag) is next.
* Found by test: the first trigger allowed a half-linked state (an intent pointing at notification A while notification B also pointed at the intent). Fixed by the agreement check above before release.
* Tests (real Postgres 16): join and read both ways, idempotent relink, one-to-one and no repointing, missing and cross-tenant targets, the database refusing the same cases without the store, purge leaving the notification, and migration down/up. Full service suite passes.

**F-02 step 3 done (the ledger pipeline now has a register row, behind a flag).** With `NOTIFICATION_LEDGER_REGISTER_ENABLED=true` (default **false**) each ledger delivery also creates a `notifications` row linked to its intent, so one communication has one identity. Code: `ledger/orchestrator_register.go`, `Orchestrator.WithRegister`, wiring in `cmd/server/main.go`.
* **Order, the safety property:** create the row (idempotent: key `ledger:<dedup key>`, purpose = template key) then link it to the intent, then set the submission marker, then call the provider, then conclude the row. If any step before the provider fails, **nothing is sent** and the intent is marked FAILED. The transport is handed the register id.
* **Outcome:** accepted becomes SENT; a refusal becomes FAILED (concluded, nothing scheduled: the ledger path has no retry and the direct retry worker must not pick these rows up); an ambiguous outcome becomes PENDING_UNKNOWN to be resolved against the provider (never FAILED, never blindly re-sent). If recording the outcome fails, the row keeps its marker and the stranded sweep turns it into UNKNOWN rather than re-sending.
* **Address provenance:** `REQUEST` when the event carried an address, `IDENTITY_CONTEXT` when resolved.
* **Resend:** a ledger-owned row is refused by the direct resend with 409 `ledger_communication_not_resendable`: the row stores the rendered body but not the stream's sender identity or headers, so a direct resend would go out from the wrong sender.
* **Webhooks:** unchanged and still correct, since the ledger attempt (with the extracted provider message id) still exists; the register row's own attempt carries the same id.
* **Cost of turning it on:** every ledger delivery writes twice, and the rendered subject and body now also sit in the register (known-gaps 97d: the register stores bodies in clear and is unbounded). Turn it on per environment, deliberately.
* Tests: 6 orchestrator unit tests (call order, failure-before-send sends nothing, unknown not failed, replay does not re-register, off means no register calls); 2 end-to-end tests against real Postgres 16 (one identity from both ends, concluded and evidenced, callback still resolves and suppresses, resend refused, replay does not duplicate; failed delivery is concluded and invisible to the retry worker); handler test for the resend refusal; config default test. Full service suite passes.
* **Not done:** existing ledger deliveries made before the flag are not backfilled (step 4); the two attempt tables are not unified (step 4); the policy gate is not yet shared (step 5).
