-- BNK-05: Cross-service evidence conflicts.
-- Owned by bank-reconciliation-svc. Written when a MATCHED statement line's
-- payment is confirmed as conflicting by payment-status-svc.
-- Inbox table provides idempotent event processing.

CREATE TABLE evidence_conflicts (
    conflict_id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    legal_entity_id           UUID NOT NULL,
    run_id                    UUID REFERENCES reconciliation_runs(run_id),
    population_id             UUID REFERENCES reconciliation_populations(population_id),
    statement_line_id         UUID NOT NULL REFERENCES statement_lines(statement_line_id),
    matched_transaction_id    VARCHAR(64),
    matched_journal_id        UUID,
    payment_id                VARCHAR(255) NOT NULL,
    provider_request_id       VARCHAR(255) NOT NULL DEFAULT '',
    bank_rec_status           VARCHAR(64)  NOT NULL,
    provider_confirmed_status VARCHAR(64)  NOT NULL,
    conflict_type             VARCHAR(64)  NOT NULL DEFAULT 'STATUS_MISMATCH',
    conflict_reason           TEXT         NOT NULL,
    conflict_status           VARCHAR(32)  NOT NULL DEFAULT 'OPEN',
    source_event_id           VARCHAR(255) NOT NULL DEFAULT '',
    raised_at                 TIMESTAMPTZ  NOT NULL DEFAULT now(),
    raised_by_principal_id    VARCHAR(255) NOT NULL DEFAULT 'system',
    resolved_at               TIMESTAMPTZ,
    resolved_by_principal_id  VARCHAR(255),
    resolution_note           TEXT,
    correlation_id            VARCHAR(255) NOT NULL DEFAULT ''
);

-- One open conflict per (statement_line_id, payment_id).
CREATE UNIQUE INDEX idx_evidence_conflicts_line_payment_open
    ON evidence_conflicts (statement_line_id, payment_id)
    WHERE conflict_status = 'OPEN';

CREATE INDEX idx_evidence_conflicts_tenant_status
    ON evidence_conflicts (tenant_id, conflict_status);
CREATE INDEX idx_evidence_conflicts_run
    ON evidence_conflicts (run_id) WHERE run_id IS NOT NULL;

ALTER TABLE evidence_conflicts ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_conflicts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_conflicts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

-- Inbox for idempotent event processing (prevents duplicate conflict rows
-- from replayed payment-status events).
CREATE TABLE evidence_conflict_inbox (
    inbox_id        UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID    NOT NULL,
    source_event_id VARCHAR(255) NOT NULL,
    processed_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_evidence_conflict_inbox_event
    ON evidence_conflict_inbox (source_event_id);

ALTER TABLE evidence_conflict_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_conflict_inbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_conflict_inbox
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
