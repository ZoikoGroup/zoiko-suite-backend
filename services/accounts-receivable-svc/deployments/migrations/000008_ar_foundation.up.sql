-- Migration: 000008_ar_foundation.up.sql
--
-- Shared plumbing for the AR-01..AR-10 modules (spec ZS-SVC-C-001 §14, §20):
--   * ar_command_idempotency       Idempotency-Key claims for state-changing commands
--   * ar_audit_events              append-only evidence of every command
--   * ar_accounting_posting_requests  durable ACC-04 posting queue (invariant 14)
--
-- Migration numbering is reserved per module so modules can be built
-- independently and still apply in dependency order:
--   0001xx AR-01   0002xx AR-02   0003xx AR-03   0004xx AR-04   0005xx AR-05
--   0006xx AR-06   0007xx AR-07   0008xx AR-08   0009xx AR-09   0010xx AR-10
--   0011xx AR/GL control populations

-- ── command idempotency ──────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ar_command_idempotency (
    tenant_id        UUID         NOT NULL,
    idempotency_key  VARCHAR(255) NOT NULL,
    operation        VARCHAR(128) NOT NULL,
    request_hash     VARCHAR(64)  NOT NULL,
    status_code      INTEGER,
    response         JSONB,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at     TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, idempotency_key)
);

ALTER TABLE ar_command_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE ar_command_idempotency FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON ar_command_idempotency;
CREATE POLICY tenant_isolation_policy ON ar_command_idempotency
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- ── append-only audit / evidence trail ───────────────────────────────────────
CREATE TABLE IF NOT EXISTS ar_audit_events (
    audit_id        UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    aggregate_type  VARCHAR(64)  NOT NULL,
    aggregate_id    VARCHAR(255) NOT NULL,
    command         VARCHAR(128) NOT NULL,
    actor_id        VARCHAR(255) NOT NULL,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    causation_id    VARCHAR(255),
    reason_code     VARCHAR(128),
    evidence_id     VARCHAR(255),
    before_state    JSONB,
    after_state     JSONB,
    detail          JSONB,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ar_audit_aggregate ON ar_audit_events (tenant_id, aggregate_type, aggregate_id, created_at);

ALTER TABLE ar_audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ar_audit_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON ar_audit_events;
CREATE POLICY tenant_isolation_policy ON ar_audit_events
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- The audit trail cannot be rewritten: UPDATE and DELETE are refused outright.
CREATE OR REPLACE FUNCTION ar_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% on % is not permitted: the record is append-only', TG_OP, TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ar_audit_events_append_only ON ar_audit_events;
CREATE TRIGGER ar_audit_events_append_only
    BEFORE UPDATE OR DELETE ON ar_audit_events
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- ── durable ACC-04 posting queue ─────────────────────────────────────────────
-- Written in the SAME transaction as the state change that has an accounting
-- consequence; a dispatcher POSTs PENDING rows to general-ledger-svc
-- /v1/postings/events, idempotent on source_event_id. Like outbox_events it is an
-- infrastructure queue read by a cross-tenant worker, so it carries no RLS.
CREATE TABLE IF NOT EXISTS ar_accounting_posting_requests (
    request_id           UUID PRIMARY KEY,
    tenant_id            UUID         NOT NULL,
    legal_entity_id      UUID         NOT NULL,
    source_type          VARCHAR(32)  NOT NULL
                         CHECK (source_type IN ('INVOICE','CREDIT_NOTE','DEBIT_NOTE','CASH_APPLICATION',
                                                'CASH_UNAPPLICATION','REFUND','WRITE_OFF')),
    source_id            VARCHAR(255) NOT NULL,
    source_event_id      VARCHAR(255) NOT NULL,
    request_payload      JSONB        NOT NULL,
    principal_id         VARCHAR(255) NOT NULL,
    correlation_id       VARCHAR(255) NOT NULL DEFAULT '',
    status               VARCHAR(16)  NOT NULL DEFAULT 'PENDING'
                         CHECK (status IN ('PENDING','IN_PROGRESS','POSTED','FAILED','QUARANTINED')),
    attempts             INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    locked_until         TIMESTAMPTZ,
    last_error           TEXT,
    posting_execution_id TEXT,
    journal_id           TEXT,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at         TIMESTAMPTZ,
    UNIQUE (tenant_id, source_event_id)
);
CREATE INDEX IF NOT EXISTS idx_ar_posting_due ON ar_accounting_posting_requests (next_attempt_at)
    WHERE status IN ('PENDING','IN_PROGRESS');
CREATE INDEX IF NOT EXISTS idx_ar_posting_source ON ar_accounting_posting_requests (tenant_id, source_type, source_id);
