# Data inventory and retention map: ZoikoSuite Assistant R0

Doc 11 F04, Doc 06 §15–§17. This lists every place the public assistant keeps data, what is in it, and how it is removed.

Retention periods come from the signed retention policy. Until the DPO signs it (DEC-06), they are dev defaults, and the service refuses to start outside dev/test without `PUBLIC_AI_RETENTION`.

## Principles

- **No raw visitor text.** Questions are never stored, logged, traced or put on events. The model sees only the redacted, contact-masked text.
- **No account, payroll, tax or customer data.** The assistant has no route to private systems (INV-01).
- **No contact details.** Emails and phone numbers are masked before retrieval and the model, and are never stored (Doc 10 §21).
- **Low-cardinality telemetry.** Metrics and traces use enums and ids only, with exporter-side allowlists.

## Stores

| Store | What it holds | What it never holds | Removed by |
|---|---|---|---|
| `public_ai.sessions` | Opaque session id, grant hash, created and expiry times, locale, market, page context, pending clarification (intent and slots only) | Visitor text, IP address, contact details | Retention purge (`PurgeSessions`) after expiry plus `PUBLIC_AI_RETENTION`; visitor `DELETE /sessions/{id}` |
| `public_ai.turns` | Request hash, status, intent, the reply envelope (approved text and citations), feedback rating and reason enums | The question text, free-text feedback | Deleted with its session |
| `public_ai.session_idempotency` | Idempotency key hash and session reference | Visitor text | With its session |
| `public_ai.usage_counters` | Token totals per day and month bucket | Anything per visitor | Not personal; kept for spend ceilings |
| `public_ai.outbox` | Allowlisted events: turn completed (request id, intent, status, latency bucket, release), handoff clicked (route key, explicit market, consent false), security signal (class, request id), release, revocation and flag events | Visitor text, contact details (field allowlist; replay rechecks it) | Published rows are purged after `PUBLIC_AI_RETENTION` (`PurgePublishedEvents`); undelivered rows are kept until delivered |
| Kafka topics and `.dlq` | The same allowlisted events | The same as the outbox | Broker retention (PENDING SRE) |
| `public_ai.releases`, `release_pointer` | Signed KB and claims releases (public approved content) | Personal data | Immutable; superseded by promotion |
| `public_ai.revocations` | Revoked source and claim ids, reason codes | Personal data | Append-only (never deleted) |
| `public_ai.control_audit` | Operator actions: action, target, actor, approval, reason, before/after hashes | Visitor data | Append-only (never deleted); retention PENDING governance |
| `public_ai.flags`, `public_ai.conflicts` | Flag values with actor and approval; claim conflicts | Visitor data | Kept as configuration |
| In-memory rate-limit buckets | Salted hash of the coarsened IP (IPv4, or IPv6 /64), with token counts | The raw IP | Swept after 10 minutes idle; lost on restart |
| Logs | Request id, route, status, latency, trace id, intent, reason, release, evidence ids | Bodies, query strings, headers, grants, visitor text | Log platform retention (PENDING SRE/DPO) |
| Traces (OTLP, when enabled) | Allowlisted attributes only: route, status, outcome, intent, releases, retrieval counts, token usage | Messages, prompts, source text, events, error messages | Collector retention (PENDING) |
| Metrics (`/metrics`) | Counters and histograms with enum labels | Session, request ids, text | Prometheus retention (PENDING) |
| Widget (browser) | The visible conversation, session id and grant in page memory only (no `localStorage`, `sessionStorage` or cookies) | Cookies, the console session | Gone on "Clear chat", page reload or tab close; sensitive inputs are replaced in the bubble |

## Open items

- Retention values for logs, traces, metrics, broker topics and the control audit (DEC-06, SRE).
- Session idle expiry (Doc 06 §17) in addition to the fixed maximum.
