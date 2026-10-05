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
| F-05 | Med, FIXED in Wave 2 slice 2 | On the governed-template path the **subject is caller free text**; templates carry no subject. Sensitive facts can reach a subject. CRLF is stripped at the SMTP layer (`sanitizeHeaderValue`), not rejected and audited. | INV-17, NP-08, NP-32 |
| F-06 | Med | `recipient_address` free text is accepted with provenance `REQUEST`. There is no regulated-notice restriction. | INV-15, NP-11 |
| F-07 | Med, FIXED in Wave 2 | Extra, undeclared variables are silently ignored; `variable_schema` is `[]string` (names only, no types, sensitivity or escape policy). Missing required variables are refused (good). | NP-07, Section 4.2 |
| F-08 | Med, FIXED in Wave 2 | BIZ-03 template state **edges are not enforced in the database**: the trigger freezes content and terminal states and a CHECK enforces maker-checker (`approved_by <> created_by`), but nothing stops a direct SQL `DRAFT` to `PUBLISHED`. | TC-02 |
| F-09 | Low, DECIDED and mitigated in Wave 2 slice 2 | `RenderPreview` renders any version (including DRAFT) with caller-supplied variables; the spec requires synthetic or authorized data only. | Section 4.5 |
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
| 07 Unexpected variable | BUILT (Wave 2) | refused on governed and built-in templates |
| 08 CRLF in subject or address | PARTIAL (Wave 2) | a governed subject refuses a value with a control character or line break; the SMTP layer still strips for free-text subjects |
| 09 Unapproved locale, regulated | PARTIAL | no silent fallback; no regulated class |
| 10 Attachment "latest" pointer | GAP | no attachments |
| 11 Free-text legal recipient | GAP | F-06 |
| 12 Recipient in another tenant | PARTIAL | RLS only |
| 13 Endpoint previously hard-bounced | BUILT (Wave 1) | both paths; but see F-12: bounces for direct sends are not matched |
| 14 Marketing opt-out | BUILT (Wave 1 step 5) | one engine for both paths; marketing (M1) is refused on the direct path and goes through the ledger pipeline |
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
| 32 Subject contains sensitive fact | BUILT (Wave 2) | a governed subject is reviewed and frozen with the body; only author-declared variables may appear in it |
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
| 48 Promo module in transactional template | PARTIAL (step 5) | the class is explicit, fixed at creation and judged by the shared engine; no content check for promotional blocks yet |
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

## 9. Wave 2 (NCD-01 intent and template control), slice 1

Scope chosen: the smallest concrete NCD-01 findings that need no decision from anyone. The larger NCD-01 items (first-class communication intents, typed variable contract, subject from typed variables, effective-dated publication) change the schema and the API and are not in this slice.

**F-07 fixed: undeclared variables are refused (NP-07).** A governed template's `variable_schema` is the reviewed list of what the wording may be given; `RenderPreview` (also used by the governed-template send) now returns `ErrTemplateVariablesUnexpected` (400 `unexpected_variables`) for any other key, naming all of them. The built-in catalogue gets the same rule (400 `unexpected_template_variables`), with an explicit `optional` list (`reason` on `rejected`) so a legitimately optional variable is still accepted; the catalogue endpoint now advertises `optional_variables`. **Behaviour change for callers:** a caller that sent extra variables, previously ignored, now gets a 400. No other service calls this one today.

**F-08 fixed: template version status edges are enforced in the database (TC-02).** Migration `000017` adds `guard_template_version_status`: a version is created as DRAFT, and only the edges the store actually uses are legal (DRAFT to REVIEW, REVIEW to APPROVED, APPROVED to PUBLISHED, PUBLISHED to SUPERSEDED or RETIRED), each only with the evidence it needs (`validated_at`, approval, `published_at`, `superseded_by_version_id`, `retired_at`). The spec's reject path (REVIEW to DRAFT) is deliberately not allowed yet: no endpoint performs it. Existing rows are not re-checked.
* Negative control recorded: with the migration removed, six illegal moves (REVIEW back to DRAFT, APPROVED back to REVIEW, APPROVED straight to RETIRED, PUBLISHED back to APPROVED or DRAFT, DRAFT to REVIEW without `validated_at`) were accepted; with it all are refused.
* Tests: store (illegal edges attacked in SQL, legal lifecycle including supersession and retirement, undeclared variables, migration down/up), templates (undeclared refused, optional accepted, the catalogue test now gives each template only what it declares), handler (400 for an undeclared variable and the provider not called).

**Not done in this slice:** F-05 (a governed template renders only a body; the subject is caller free text), F-09 (the preview endpoint renders any version, including drafts, with caller-supplied variables; limiting it to synthetic data or a distinct authorization is an authorization-model decision), communication intents, typed variables, attachments, locale policy for regulated notices.

### Wave 2 slice 2: decisions (taken by the implementer on the owner's delegation, 2026-10-05) and F-05, F-09

**F-05, decision: a template version owns a reviewed subject.** A version gets an optional `subject` (text with `{{.variable}}` placeholders only: no conditionals, loops, pipelines, functions or nested fields) and `subject_variables`, the list of variables its author declares safe to appear in a subject. The list must be a subset of `variable_schema` and the subject may reference nothing outside it; a reviewer sees the list. Both are frozen with the version by extending the 000005 immutability trigger (migration `000018`), with CHECKs for shape (1 to 200 characters, one line), declaration and "variables need a subject".
* **Send path:** a version with a subject owns it, and a caller-supplied subject is refused (400 `conflicting_content`): reviewed wording cannot be sent under unreviewed text. A version without one is legacy: the caller still supplies it (400 `missing_fields` if absent), so nothing existing changes.
* **Rendering:** a missing variable is refused, and a value containing a control character or line break is refused with 400 `invalid_subject` (not stripped, NP-08); an over-long result is refused.
* **Why this shape and not typed variables:** there is no sensitivity metadata on variables yet (Y-001 NCD-01 typed variable contract). Until there is, "the author declares which variables are safe for a subject, and a reviewer approves that list" is the strongest control that does not invent a classification. Replace it with sensitivity-driven checks when the typed contract exists.
* **Bug caught by the full suite before release:** the first version of the CHECK rejected any version created with no `variable_schema` (stored as JSON `null`). Fixed: an empty subject-safe list is always valid.

**F-09, decision: no new authorization action; preview shows placeholders.** The preview endpoint is already gated by `TEMPLATE_MANAGE` (template authors only), and a new action would break existing grants for no gain. The real exposure was authors being pushed to use real data to check wording, so a preview now fills any declared variable not supplied with a visible marker (`[variable]`, listed in `placeholders_used`) instead of refusing. The send path is unchanged: a missing variable is still refused there.

**Identity plan step 4, decision: keep the ledger `delivery_attempts` as the system of record for ledger attempts; no backfill, no folding.** New ledger deliveries already have a linked register row with its own attempt (step 3); folding the old table into the register would duplicate evidence and risk, and deliveries made before the flag was turned on stay ledger-only, which is the truthful state (known, stated, and visible by the missing link). Step 4 therefore needs no code. Next in that plan: step 5, one policy gate for both paths.

Tests: domain (subject parse and refusals, declaration checks, rendering and header-injection refusals), store against real Postgres (stored and rendered, frozen by the database, bad shapes refused by CHECK, legacy version unaffected, unsafe subjects refused, migration down/up), handler (reviewed subject sent and no salary in it, caller subject refused, header-injecting value refused, unsafe declarations refused, legacy still works, preview markers). Full service suite passes.

## 10. Identity plan step 5: one policy gate, with an explicit communication class

**Decision (implementer, on the owner's delegation):** the direct send path states what KIND of message it is, and is judged by the same precedence engine as the ledger pipeline.
* **`communication_class`** (migration `000019`) on `notifications`: S0 security, T0 transactional, A1 operational, L1 lifecycle, M1 marketing (the classes the engine already uses). NULL means none stated and is judged as T0, which is how every direct send was treated before. A CHECK limits the values; a trigger **fixes the class at creation** (it decides what can block a message, so a retry, a resend or an UPDATE cannot reclassify it, for example from marketing to transactional to slip past an opt-out).
* **The direct API accepts S0, T0 and A1 only.** M1 and L1 are a 400 (`invalid_communication_class`) and are also refused by the guard if a row somehow carries them: marketing and lifecycle mail need the stream's sender identity and RFC 8058 one-click unsubscribe, which only the ledger pipeline provides, and a direct send must not be a way around them (INV-07).
* **`DirectSendGuard` now takes the precedence engine** (`policy.PolicyResolver`) instead of a bare suppression checker, and builds the stream from the class (`ledger.StreamForDirectClass`). A unit test pins every seed template's class/stream pair to that mapping so they cannot drift. The rules (suppression, a class the engine does not know fails closed) are therefore the same code on both paths.
* **The ledger register row** (step 3) records the template's class, so both paths' rows carry the same field.
* **Behaviour change:** the guard's refusal reason now names the rule (`refused by delivery policy (SUPPRESSION_ENFORCED): ...`).
* Proven against real Postgres with the real suppression store: an unsubscribe does **not** stop an S0, T0 or unclassified notice and **does** stop an A1; a hard bounce stops every class; M1 never reaches the provider; the class is stored, read back, frozen and constrained by the database.
* **Not done:** the engine still has no per-user preferences, quiet hours, privacy (PRV) or jurisdiction (PDC) inputs, so "one gate" means one rule set, not yet the full NCD-02 decision. The Y-001 purpose-class names (SECURITY_CRITICAL, ...) are not used; the ledger's S0/T0/A1/L1/M1 are the vocabulary until NCD-01 defines the intent catalogue.

## 11. Identity plan step 6: one communication identity in the events

**Done (additive, no migration).** Every `notification.sent`, `notification.failed`, `notification.outcome_unknown`, `delivery.attempt.created` and `delivery.attempt.unknown` event now names the communication the same way, built by one helper (`events.buildFor`):

| field | meaning | when present |
|---|---|---|
| `communication_id` | the stable id of the logical communication (the register id) | always |
| `notification_id` | the same value, kept for consumers written before `communication_id` | always |
| `message_intent_id` | the ledger intent the message was produced from | only when linked (omitted, never blank, for a direct send) |
| `communication_class` | S0/T0/A1/L1/M1 | only when stated |

* No existing field changes meaning or moves, and the Kafka key is still the communication id, so one communication stays ordered on one partition. The payload still carries no subject, body or address.
* `domain.Notification` gained `message_intent_id` (read from migration 000016's link), so `GET /v1/notifications/{id}` also shows which intent a notification came from.
* Tests: unit tests per builder (identity present, optional fields omitted when absent, earlier fields unchanged, still no content or address); two real-Postgres tests: a ledger delivery's events all name the same communication, the intent and the class, and a direct send's events omit the intent.
* **Known remaining gaps in the event contract:**
  * With `NOTIFICATION_LEDGER_REGISTER_ENABLED` off (the default), a ledger delivery leaves **no events at all**, so consumers cannot see it. The identity exists only for deliveries made with the flag on.
  * The spec's canonical names (`communication.prepared`, `communication.blocked`, `delivery.evidence.recorded`, `endpoint.suppressed`, `notice.*`) are not emitted; the service publishes `notification.*` and `delivery.attempt.*`. Introducing them is a contract decision for consumers, best made before any consumer exists.
  * `event_outbox_event_known` (migration 000010) lists the allowed event types; a new type needs a migration.
  * The comments reference an `asyncapi.yaml` that does not exist in the repository.

**Identity plan status:** steps 1 to 6 are done or decided. What the plan does not cover: turning `NOTIFICATION_LEDGER_REGISTER_ENABLED` on, which is an operational decision per environment (it doubles ledger writes and puts rendered content in the register).

## 12. Wave 2 slice 3: the communication intent registry (NCD-01)

**Built (migrations `000020`, `000021`).** `POST/GET /v1/communication-intents...` (section 4.5), with the template roles for authorization (`TEMPLATE_MANAGE` to author, `TEMPLATE_APPROVE` to approve, publish and retire; a new action would have to be granted everywhere for no gain).

* **An intent** is a stable, server-assigned identity (`intent_key` such as `payroll.payslip_available`, unique per tenant, never derived from a display name; the database makes the identity immutable and undeletable). **A version** carries `purpose_class` (S0/T0/A1/L1/M1), `evidence_class` (E0 to E4), `allowed_channels`, `marketing_allowed`, `record_requirement` and a **typed variable contract**.
* **Typed contract:** every variable declares a `type` (STRING with a length, NUMBER as a decimal string, DATE, ID, URL as https without credentials), `required`, and a **`sensitivity` (S0 to S3) that is never defaulted**. Values are checked on send: control characters, exponent numbers, `http://` and `javascript:` URLs, over-long strings, missing required and undeclared variables are all refused, and every problem is reported at once (NCD-004).
* **Lifecycle:** DRAFT, REVIEW, APPROVED, PUBLISHED, with the maker-checker as a CHECK, content frozen from the moment of writing, and illegal edges refused by a trigger. A change of purpose class is a new version. `marketing_allowed` is only possible for lifecycle (L1) or marketing (M1) purposes (INV-07), by service and by CHECK.
* **Effective-dated and bitemporal (section 9.2):** a version is published with an `effective_from` that is never in the past and is later than every version already published, so "the version in force" is never ambiguous. `GET .../effective?at=&known_at=` resolves what was in force at transaction time `at` as the platform knew it at `known_at`. Retiring an intent ends its future but not its history: it blocks resolution only when the retirement is at or before both times, so a message from before the retirement still reconstructs exactly.
* **Binding:** a template definition can be bound to an intent at creation, for good (a trigger refuses rebinding, unbinding or later binding, and cross-tenant binding even if the service is bypassed), so a reviewed template cannot be moved under a more permissive intent. The bound intent must be active, of the same tenant and legal entity. Wording is authored against the contract in force now: it may only use variables the intent governs, and **a subject may only use variables whose sensitivity is S0 or S1** (INV-17 by the contract, replacing the earlier "author declares subject-safe variables" stopgap for bound templates). A bound template cannot be authored before its intent has a version in force.
* **Enforcement at send:** a bound template is sent under the intent version in force NOW and nothing else. The purpose class comes from the intent (a different one from the sender is refused), the channel must be one the intent allows (NCD-011), a purpose the direct path does not offer (M1, L1) is refused with a pointer to the ledger pipeline, the template must still conform, and every variable must satisfy its declared type. **Fail closed:** no registry, or no version in force, means no send. The notification is **pinned to the exact intent version** (`intent_version_id`, fixed at creation by a trigger), and its events carry it (INV-04).
* **Bugs caught on the way:** the test harness's table-reset list did not include the new tables; and the migration test showed the real rollback order (000021 before 000020).

**Not built, deliberately:**
* **Evidence classes are recorded and carried, not enforced** against a provider's capability: their exact minimums per channel are open decision OD-06.
* **Template sets** (one exact template version per channel and locale inside an intent) and **attachment contracts** (DRC record slots): templates are bound to an intent, but the intent does not enumerate them.
* **Re-validation on drift:** if a later intent version drops a variable that an already-published template uses, the send is refused (fail closed) at the next send, but nothing flags the template in advance, and revalidation against a newly effective contract is covered by the send-time check, not tested with a real clock.
* **No intent events** (`intent.published` and so on): the outbox allow-list needs a migration and the event contract is undecided.
* **Reason codes:** the NCD-nnn codes appear in messages and in one error code; there is no full stable-code catalogue yet.
* **Typed contract for unbound templates and the ledger catalogue is not applied:** only templates bound to an intent are governed by it; the built-in catalogue and ledger Go templates keep their own rules.
* **Variable sensitivity is not yet used to keep S2/S3 values out of logs, previews or provider metadata**; it governs subjects only.

## 13. Wave 3 slice 1: the privacy permission gate (NCD-02 5.3)

Sending an email uses the recipient's contact details for a purpose, and `privacy-decision-svc` is the authority on whether that use is permitted. Until now this service never asked. The standard is explicit: personal-data use needs PERMIT (or RESTRICT with the restrictions satisfied); INDETERMINATE fails closed; no operational mode may turn BLOCK or INDETERMINATE into permission (INV-30, NP-17, NP-18).

* **Where:** inside the direct-send guard, the last point before the provider, so the first attempt, every retry and every resend are asked again (a consent withdrawn since creation is honoured on the retry). It runs after the local controls (kill switch, class, suppression, precedence), so the remote question is asked only for a message that would otherwise go out.
* **What is asked:** the privacy activity and purpose named by the **intent version** the notification is pinned to (`privacy_activity_id`, `privacy_purpose_id`, both or neither, frozen with the version by the immutability trigger; the sender never chooses them), about the recipient principal, operation `USE`, data category contact details.
* **Verdicts:** PERMIT sends. RESTRICT is **refused**, because a restriction is a duty this service cannot perform and it will not pretend to by sending anyway. BLOCK and REVIEW_REQUIRED are terminal refusals (NCD-008). INDETERMINATE, a timeout, an unreachable service, a non-200, an unreadable or unrecognised answer, an answer without a decision id, and an unreadable intent version are **retryable refusals** (fail closed). An intent version with no privacy binding is a terminal refusal: a person-directed message is never sent on an assumption.
* **Evidence:** every attempt records `privacy_decision_id` and `privacy_result` (PERMIT, RESTRICT, BLOCK, REVIEW_REQUIRED, INDETERMINATE, UNAVAILABLE, NOT_BOUND), including refusals; both are on `GET .../attempts` and the `delivery.attempt.created` event (omitted when blank). Migration 000022 (rollback order 000022, 000021, 000020).
* **Switch:** `NOTIFICATION_PRIVACY_ENFORCEMENT`, default **off**, because turning it on starts refusing intent-bound sends whose intent has no binding. With it on, `PRIVACY_DECISION_URL` is required (the service refuses to start without it); `PRIVACY_DECISION_TIMEOUT` defaults to 3s.
* **Tests:** client over a real HTTP server (headers, body, every failure shape), the verdict table (only PERMIT allows), gate and guard fakes (refusal never reaches the provider; local refusals come first), config, and real-Postgres tests for binding validation, binding immutability at the database, attempt evidence round trip, rejection of an unknown result, and migration 000022 down/up. Full suite passes.

**Not built, deliberately:**
* **Legacy sends are not gated:** a notification with no intent (no `intent_version_id`) has no binding to enforce, so it passes untouched. Closing that means requiring an intent for every send, a separate decision.
* **IN_APP is not gated**, as the guard as a whole only covers EMAIL; the ledger pipeline does not call this gate yet.
* **RESTRICT is always refused.** Honouring specific constraint types (minimise, redact) needs the content pipeline to cooperate.
* **The sender principal is used as the actor** on the decision request; there is no service identity distinct from the caller yet.
* **No preferences, quiet hours or recipient channel decision yet** (the rest of NCD-02), and no call to the PDC (data-classification) service.
* **No mutual TLS or retry/backoff of the privacy call itself:** a failure becomes a retryable refusal and the existing retry worker asks again.
