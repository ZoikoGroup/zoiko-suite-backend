-- Idempotency for BIZ-09 material writes. The canonical service input
-- contract (ZS-ARCH-SVC-001 §4, INV-08) makes Idempotency-Key mandatory on
-- material writes; until now it was required by the envelope middleware but
-- never honoured, so a retried AddComment/Mention/AttachReference created a
-- duplicate row.
--
-- One row per (tenant, principal, operation, key). The row is claimed inside
-- the same transaction as the mutation it protects and carries that
-- mutation's result, so an exact replay returns the original result and a
-- concurrent replay blocks on the unique key rather than inserting twice.
-- request_hash fingerprints operation + target + body: reusing a key with a
-- different request is refused.
--
-- Tenant-scoped under FORCE row-level security like every other table here.
-- Not append-only: result is written once, after the claim, in the same
-- transaction.

CREATE TABLE idempotency_keys (
    tenant_id       TEXT        NOT NULL,
    principal_id    TEXT        NOT NULL,
    operation       TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    request_hash    TEXT        NOT NULL,
    result          JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, principal_id, operation, idempotency_key)
);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON idempotency_keys
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
