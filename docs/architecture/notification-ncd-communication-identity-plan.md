# ZS-SVC-Y-001: plan for a single communication identity (audit finding F-02)

Status: **plan only, nothing built, needs a decision.** Date: 2026-10-05. Follows `notification-ncd-wave0-audit.md`.

## The problem
`notification-svc` has two send paths. They share a transport (`Deliverer`) and the event outbox, and nothing else:

| | Direct path | Ledger pipeline |
|---|---|---|
| Entry | `POST /v1/notifications` | `POST /v1/notifications/events/ingest` |
| Identity | `notifications.notification_id` | `message_intents.message_intent_id` |
| Attempts | `notification_delivery_attempts` (000011) | `delivery_attempts` (000007) |
| Evidence | outbox events | `delivery_events` |
| Retry, unknown, resend, stranded sweep | yes | no |
| Suppression, kill switch, class policy | suppression and kill switch now (F-03, Wave 1); no class | yes, with class |
| Webhook callbacks match | **no** (see F-12) | yes |

Y-001 INV-02 requires one `communication_id` per logical business communication, with retries and fallbacks as linked attempts. Today the outbox events already call the direct path's `notification_id` the `communication_id`, but the ledger pipeline never produces one.

**F-12 (found while planning; FIXED, see the audit, section 8):** `LookupAttemptByProviderMessageID` searches only `delivery_attempts` (ledger). A direct-path send records no `provider_message_id` anywhere a webhook can find it, so a bounce or complaint for a direct send cannot be matched to its notification and goes to the DLQ as unresolved. Hard bounces from direct sends therefore never create suppressions, which makes the F-03 guard weaker than it should be.

## Options

**A. Make `notifications` the canonical communication and have the ledger path write into it.**
Each ledger send creates a `notifications` row (and `notification_delivery_attempts`) and links `message_intents.notification_id`. Attempts, retry, unknown and resend then apply to both. Webhook lookup gains `notification_delivery_attempts`.
* Pros: reuses the richest, best-tested machinery; the events already use this id; smallest new schema.
* Cons: `notifications` carries subject and body in clear (known-gaps 97d); the ledger's render and class concepts have to be added as columns or a side table; the table name says "notification", not "communication".

**B. New `communications` table that both paths write to, with both existing tables linking to it.**
* Pros: matches Y-001 section 9.1 (`Communication`, `DeliveryJob`, `DeliveryAttempt`) directly and gives a place for intent version, policy snapshots and purpose class.
* Cons: largest change; every existing row needs a backfill; two attempt tables still have to be reconciled.

**C. Retire the direct path** and route everything through the ledger pipeline, moving retry/unknown/resend onto it.
* Pros: one path, one policy engine.
* Cons: the ledger path is event-driven and template-catalogue driven, so free-text and governed-template sends have no home; the ledger has no unknown-outcome or resend mechanism; most work and most risk.

## Recommendation
**A now, B as the target shape.** Make `notifications.notification_id` the `communication_id` (it already is, in the events) and add `communications`-style columns to it over time. Concretely, in order, each step its own migration and release:

1. **DONE: Webhook lookup (F-12):** record a `provider_message_id` on `notification_delivery_attempts` (new nullable column, written from the SMTP provider's message id) and make `LookupAttemptByProviderMessageID` search it too. Independent of the rest, high value, no data move.
2. **DONE: Link column:** add nullable `message_intents.notification_id` and `notifications.message_intent_id`. No behaviour change.
3. **DONE (behind NOTIFICATION_LEDGER_REGISTER_ENABLED, default off): Ledger path writes a `notifications` row** for each delivery (same transaction as the intent), so every send has a register row, attempts and retry. Behind a config flag, default off, until step 4 is proven.
4. **Backfill and unify attempts:** copy existing ledger `delivery_attempts` into `notification_delivery_attempts` (or keep the ledger table as a read-only history and stop writing it). Decide before this step.
5. **One policy gate:** run the class-aware precedence engine in front of both paths (the direct path then gets a real class via an optional `purpose_class` request field, replacing the T0 default in `DirectSendGuard`).
6. Update Y-001 events to carry one `communication_id` for both.

Each step is reversible until step 4. Steps 1 to 2 are safe to do first.

## Decisions needed
1. **A, B or C?** (This plan assumes A.)
2. After step 3, is the ledger `delivery_attempts` table kept as the system of record for ledger sends, or folded into `notification_delivery_attempts`?
3. Does the direct path stay a supported public API long-term (free-text and governed-template sends), or is it a transitional path to be retired once callers use intents?
4. The register stores subject, body and recipient address in clear; decide the data-minimisation approach (known-gaps 97d) before more traffic is routed through it.

## Risks
* Step 3 doubles writes for ledger sends; it needs the same RLS and tenant handling as the direct path.
* Any change to what `communication_id` means in published events is a contract change for consumers; none exist yet (no other service calls this one), which is the cheapest time to do it.
* Backfilling attempts can duplicate provider evidence if run twice; use a stable mapping key.
