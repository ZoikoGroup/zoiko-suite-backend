-- The canonical service input contract (ZS-ARCH-SVC-001 v2.0 §4, INV-08)
-- requires Idempotency-Key on every material write to this service, and the
-- envelope middleware already refuses a POST that omits the header — but
-- until this migration the service never stored the key anywhere. A retried
-- CreateMetricDefinition/PublishNewVersion call (e.g. after a client-side
-- timeout on a request that actually succeeded server-side) silently
-- created a second metric definition row (PublishNewVersion) or depended on
-- metric_code's own unique index to coincidentally reject it with a 409
-- (CreateMetricDefinition) instead of returning the original 201/200 — the
-- exact failure INV-08 exists to prevent. Same pattern as
-- schema-registry-svc's 000004_add_idempotency_key migration.
ALTER TABLE report_metric_definitions
    ADD COLUMN idempotency_key VARCHAR(255);

-- Partial (WHERE idempotency_key IS NOT NULL) because existing rows predate
-- this column and are NULL, and NULL never collides with NULL under a
-- unique index — correct for history written before replay protection
-- existed, not something to backfill.
--
-- Scoped to (metric_code, idempotency_key) rather than idempotency_key
-- alone: the key is caller-generated per INV-08 and only has to be unique
-- within the metric_code it protects, the same scoping schema-registry-svc
-- uses for (event_name, idempotency_key).
CREATE UNIQUE INDEX idx_report_metric_definitions_code_idempotency
    ON report_metric_definitions (metric_code, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
