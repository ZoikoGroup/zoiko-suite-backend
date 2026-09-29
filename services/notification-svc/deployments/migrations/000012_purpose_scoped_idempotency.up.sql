-- Purpose-scoped idempotency for POST /v1/notifications (ZS-SVC-Y-001 §3.4).
--
-- §3.4: "Every logical communication has a stable communication_id and
-- purpose-scoped idempotency key derived from the originating business event/
-- workflow action." Until now the only key was (tenant_id, correlation_id), so
-- two DIFFERENT communications raised by one business event — a payslip notice
-- and a tax-form notice for the same payroll run, sharing its correlation id —
-- collided, and the second was silently answered with the first.
--
-- OPTIONAL, BY RULING (29 Sep 2026). Making purpose_context required would
-- break every service that sends notifications today. So:
--
--   * A send WITH a purpose (or an explicit idempotency_key) is keyed on
--     (tenant_id, idempotency_key), where the key is derived from tenant, legal
--     entity, correlation, purpose, recipient and channel unless the caller
--     supplies one.
--   * A send WITHOUT either keeps exactly the old behaviour: keyed on
--     (tenant_id, correlation_id).
--
-- The old unique index is replaced by a PARTIAL one covering only rows with no
-- idempotency key, so a purpose-scoped communication is no longer blocked by an
-- unrelated one that happens to share its correlation id, while every existing
-- row (all of which have a NULL key) stays exactly as constrained as before.
-- Both indexes are unique and partial, and the store names the matching one in
-- ON CONFLICT, so dedup stays atomic in the database — no check-then-insert.

ALTER TABLE notifications ADD COLUMN IF NOT EXISTS purpose_context VARCHAR(255);
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(500);

DROP INDEX IF EXISTS idx_notifications_tenant_correlation;
CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_tenant_correlation_unkeyed
    ON notifications (tenant_id, correlation_id) WHERE idempotency_key IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_notifications_tenant_idempotency
    ON notifications (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
