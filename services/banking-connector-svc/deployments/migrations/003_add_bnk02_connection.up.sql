-- BNK-02 Bank Connection: the real consent/scope/token/expiry/health/
-- revocation lifecycle. Extends the existing bank_connections table in
-- place — this table already owns "the connection concept" (status,
-- health), it just never implemented consent at all.
--
-- Backward compatibility: the existing CreateConnection command keeps
-- creating rows with status='CONNECTED' unchanged — this migration does
-- not touch that path or its default. The new commands
-- (InitiateConnection/CompleteConnectionAuthorization/...) operate on the
-- richer REQUESTED->AUTHORIZING->ACTIVE->DEGRADED/SUSPENDED->
-- RECONSENT_REQUIRED->REVOKED lifecycle. No CHECK constraint exists on
-- `status` today, so both sets of values coexist without a migration to
-- widen an enum.
--
-- bank_account_id references treasury-svc's BNK-01 bank_accounts row by
-- ID — a different service/database, so this is a plain column, not a
-- real FK; validity is checked live via internal/clients/bankaccount.go
-- at CompleteConnectionAuthorization time, not enforced at insert time.

ALTER TABLE bank_connections
    ADD COLUMN bank_account_id VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN provider_ref VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN consent_scope TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN token_lease_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN token_expires_at TIMESTAMPTZ NULL,
    ADD COLUMN health_status VARCHAR(32) NOT NULL DEFAULT 'UNKNOWN',
    ADD COLUMN region VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN correlation_id VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN created_by_principal_id VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN suspend_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN revoke_reason TEXT NOT NULL DEFAULT '';

-- Idempotency-by-correlation-id for InitiateConnection — a retried
-- request returns the original row rather than creating a second one.
CREATE UNIQUE INDEX idx_bank_connections_tenant_correlation
    ON bank_connections (tenant_id, correlation_id) WHERE correlation_id <> '';

-- bank_connection_events: append-only audit trail for the full consent
-- state machine — every transition (including REVOKED, which must be
-- provable/verifiable per the spec's own negative path "revoked consent
-- still used").
CREATE TABLE bank_connection_events (
    event_id           VARCHAR(64) PRIMARY KEY DEFAULT gen_random_uuid()::text,
    connection_id       VARCHAR(64) NOT NULL REFERENCES bank_connections (connection_id),
    tenant_id           VARCHAR(64) NOT NULL,
    event_type          VARCHAR(64) NOT NULL,
    detail               TEXT NOT NULL DEFAULT '',
    actor_principal_id VARCHAR(128) NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_bank_connection_events_connection ON bank_connection_events (connection_id, created_at ASC);

ALTER TABLE bank_connection_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_connection_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_connection_events
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Append-only: no UPDATE/DELETE, ever. The runtime connects as Postgres
-- superuser (bypasses GRANT/REVOKE), so only a BEFORE trigger actually
-- enforces this — same documented caveat as every other service in this
-- codebase's Banking domain.
CREATE OR REPLACE FUNCTION reject_connection_event_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'bank_connection_events rows are append-only and can never be updated or deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_connection_event_mutation
    BEFORE UPDATE OR DELETE ON bank_connection_events
    FOR EACH ROW EXECUTE FUNCTION reject_connection_event_mutation();

-- Revoked consent must never be silently reactivated — the spec's own
-- named negative path. REVOKED is terminal: no further mutation of the
-- connection row is permitted once revoked, full stop.
CREATE OR REPLACE FUNCTION reject_revoked_connection_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'bank_connections rows are never deleted';
    END IF;
    IF OLD.status = 'REVOKED' THEN
        RAISE EXCEPTION 'bank connection % is REVOKED and can never be modified or reactivated', OLD.connection_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_revoked_connection_mutation
    BEFORE UPDATE OR DELETE ON bank_connections
    FOR EACH ROW EXECUTE FUNCTION reject_revoked_connection_mutation();
