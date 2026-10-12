-- The canonical service input contract (ZS-ARCH-SVC-001 v2.0 §4, INV-08)
-- requires Idempotency-Key on every material write to this service and
-- states its purpose plainly: "duplicate/replay protection". The envelope
-- middleware already refuses a POST /versions call that omits the header,
-- but until this migration the service never stored the key anywhere — a
-- retried registration (e.g. after a client-side timeout on a POST that
-- actually succeeded server-side) silently claimed a brand new version
-- number instead of returning the original one, which is the exact failure
-- INV-08 exists to prevent. Same pattern as accounts-payable-svc's
-- 000002_add_idempotency_index migration.
ALTER TABLE event_schemas
    ADD COLUMN idempotency_key VARCHAR(255);

-- Partial (WHERE idempotency_key IS NOT NULL) because existing rows
-- predate this column and are NULL, and NULL never collides with NULL
-- under a unique index — that is the correct behavior for history written
-- before replay protection existed, not something to backfill.
--
-- Scoped to (event_name, idempotency_key) rather than idempotency_key
-- alone: the key is caller-generated per INV-08 and only has to be unique
-- within the registration it protects, the same way correlation_id is
-- scoped to tenant_id in accounts-payable-svc rather than global.
CREATE UNIQUE INDEX idx_event_schemas_event_idempotency
    ON event_schemas (event_name, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
