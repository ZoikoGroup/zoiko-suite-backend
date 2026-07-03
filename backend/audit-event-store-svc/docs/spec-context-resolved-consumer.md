# Service Spec — `identity.context.resolved` Consumer
## Audit Event Store Service

**Author:** @evidence  
**Date:** 2026-07-03  
**Branch:** `feat/audit-event-store-context-resolved`  
**Status:** Implemented, tests passing, PR open

---

## 1. Context

`identity-context-svc` publishes `identity.context.resolved` on the
`zoiko.identity.events` topic whenever an identity/session resolution
succeeds.  The Audit Event Store is a read-only evidence consumer of
every other domain's events.  This spec documents how it absorbs the
`ContextResolved` event.

---

## 2. Owned Objects

### Table: `audit_events` (single shared table — no new table)

Both `entity.status.changed` (existing) and `identity.context.resolved`
(this change) share the same structural envelope:

| Column           | Type        | Source                                  |
|------------------|-------------|-----------------------------------------|
| `event_id`       | TEXT PK     | Broker-assigned unique occurrence ID    |
| `event_type`     | TEXT        | `"identity.context.resolved"`           |
| `tenant_id`      | TEXT        | Payload `tenant_id` (mandatory)         |
| `legal_entity_id`| TEXT        | Payload `legal_entity_id` (mandatory)   |
| `principal_id`   | TEXT NULL   | Payload `principal_id` (mandatory here) |
| `source_service` | TEXT        | Envelope `source_service`               |
| `schema_version` | TEXT        | Envelope `schema_version`               |
| `payload`        | JSONB       | Full payload (incl. session_context_id, correlation_id) |
| `stored_at`      | TIMESTAMPTZ | `DEFAULT NOW()` — DB clock only         |

**Why single table?** Both event types share identical column structure.
Separate tables would duplicate the schema, fragment indexes, and add
a JOIN when querying "all audit events for a tenant". The JSONB
`payload` column absorbs any per-type fields (e.g. `session_context_id`,
`correlation_id`) without schema proliferation.

---

## 3. ContextResolved Payload Shape

From `identity-context-svc/internal/events/publisher.go`:

```json
{
  "principal_id":       "string (required)",
  "tenant_id":          "string (required)",
  "legal_entity_id":    "string (required)",
  "session_context_id": "string (required)",
  "correlation_id":     "string (required)"
}
```

All five fields are required. Absence of any field causes rejection (see §5 Failure Modes).

---

## 4. Idempotency Approach

**Single atomic SQL statement — no separate existence check:**

```sql
INSERT INTO audit_events
    (event_id, event_type, tenant_id, legal_entity_id, principal_id,
     source_service, schema_version, payload)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (event_id) DO NOTHING
```

`ON CONFLICT (event_id) DO NOTHING` is the **only** safe dedup pattern.
A prior `SELECT EXISTS(...)` + conditional INSERT has a TOCTOU race:
two concurrent deliveries can both observe "not exists" before either
inserts, producing a duplicate row. With `ON CONFLICT DO NOTHING` the
Postgres PRIMARY KEY constraint serialises the decision atomically.

The in-memory `FakeStore` replicates this via a `sync.Mutex`-protected
map — existence check + insert happen under the same lock.

---

## 5. Failure Modes

| Condition                            | Action                                                |
|--------------------------------------|-------------------------------------------------------|
| `event_id` is empty                  | Log error, return nil — no storage, no crash          |
| Envelope JSON unparseable            | Log error, return nil — no storage, no crash          |
| Required envelope field missing      | Log error, return nil                                 |
| Payload unmarshal failure            | Log error, return nil — no partial storage            |
| Any required payload field missing   | Log error, return nil — no partial storage            |
| Duplicate `event_id`                 | Silent no-op (ON CONFLICT DO NOTHING) — return nil    |
| Unknown `event_type`                 | Log warn, return nil — not requeued                   |
| DB transient error                   | Return error to caller — broker retries / DLQ routing |

---

## 6. Doctrine Compliance

| Rule                                         | Status |
|----------------------------------------------|--------|
| No UPDATE or DELETE on stored events         | ✅ INSERT only |
| Idempotency via atomic INSERT ON CONFLICT    | ✅ No SELECT+INSERT pattern |
| tenant_id + legal_entity_id on every row     | ✅ Promoted to top-level columns |
| Evidence service never mutates source truth  | ✅ Read-only consumer |

---

## 7. Tests

| Test                              | What it proves                                    |
|-----------------------------------|---------------------------------------------------|
| TestDedupSameEventIDTwice         | Same event_id x2 → exactly 1 row, no error        |
| TestDedupConcurrent               | Two goroutines, same event_id → 1 row, no error   |
| TestMalformedEventRejected (x7)   | All malformed variants rejected cleanly           |
| TestEntityStatusChangedStored     | Existing consumer unaffected                      |
| TestContextResolvedStoredCorrectly| All fields stored correctly                       |
