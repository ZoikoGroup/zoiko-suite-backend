# workflow-history-svc (:8097)

Read-only evidence consumer. It consumes workflow-svc's five published events
(`workflow.started`, `approval.granted`, `approval.rejected`, `workflow.escalated`,
`workflow.completed`) into an append-only, tenant-isolated table and serves them
back. Authority: Microservices Specification §14.3 (purpose), Data Model §15
(`WorkflowTransition`), ZS-API-001 (API baseline), the Event Catalogue standard.
`docs/audit_files/Evidence, Audit & Utility.md` is an older audit report and is
not authoritative; its write route, reconstruct/summary routes, `WORKFLOW_READ`/
`WORKFLOW_WRITE` permissions and sequence counter are not implemented.

## Routes

| Route | Notes |
|---|---|
| `GET /v1/workflows/{workflow_instance_id}/history` | One instance's events. 404 if none. |
| `GET /v1/workflows/history?legal_entity_id&from&to` | Window query for a tenant and legal entity. |

Both authorize `WORKFLOW_HISTORY_READ` against the legal entity (403 denied, 503
when authorization-svc is unreachable) and take the tenant only from the verified
`X-Tenant-Id`.

## Ordering

Both routes order by `(recorded_at, event_id)` (ZS-API-001 §15.1: a deterministic
tie-breaker), so rows sharing a timestamp always come back in the same order.

## Pagination (opt-in) and compatibility

ZS-API-001 §15.2 cursor pagination is available on both routes, **opt-in**:

- `page_size` (1-200, default 50 when only `cursor` is sent) and/or `cursor`.
- Response: `{"data": [...], "meta": {"page_size": N, "next_cursor": "..."}}`;
  `next_cursor` is absent on the last page. The cursor is opaque and bound to the
  query (tenant, instance or entity+window) it was issued for. It is not signed:
  no signing key facility exists in this service, and every page re-applies
  tenant scoping and legal-entity authorization, so an edited cursor can only
  move within the caller's own authorized data. A cursor past the end is an empty
  page (200); only a first request can be a 404.
- Invalid `page_size` or `cursor`: 400 `{"error": "..."}`.

A request with **neither** parameter keeps the original response: a bare JSON
array of every matching event. This is deliberate. evidence-manifest-svc's
aggregator calls both routes with no pagination parameters and decodes a bare
array; a default page of 50 would silently truncate an evidence manifest, and an
object body would make every manifest generation fail. The result is that
unparameterized calls are still unbounded. Closing that needs a change in
evidence-manifest-svc (send `page_size`, follow `next_cursor`, accept the
`data`/`meta` envelope), after which the unparameterized form can be retired.

## Envelope tenant integrity

workflow-svc stamps `tenant_id` on the event envelope (Doc 03 §19). When it is
present it must equal the tenant the consumer would store the event under (the
payload tenant for `workflow.started`; the tenant inherited from the stored
`workflow.started` row for every other event). A mismatch stores nothing and
returns `ErrEnvelopeTenantMismatch`; the Kafka runner retries, then dead-letters
the message unchanged (the quarantine). An envelope with no tenant cannot be
checked and is accepted as before (the field is `omitempty` in the producer).

## Tests

```bash
cd services/workflow-history-svc
go vet ./... && go build ./...
go test ./...                       # default suite
go test -tags integration ./...     # embedded-Postgres suite (also covers the real queries)
```
