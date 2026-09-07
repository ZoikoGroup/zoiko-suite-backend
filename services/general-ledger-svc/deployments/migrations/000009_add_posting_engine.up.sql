-- Migration: 000009_add_posting_engine.up.sql
--
-- ACC-04 (Posting Engine): "owns Posting execution/calculation/rule
-- consequence. Must never own: Source business fact, tax determination."
-- Fuller ownership: "PostingExecution, calculation trace, rule
-- resolution, posting batch and consequence uniqueness record; ledger
-- entries are committed to ACC-05."
--
-- In this platform's own deployment grouping, ACC-05 (posted ledger
-- truth) is general-ledger-svc's existing journal_headers/journal_lines
-- — the same service ACC-04 is being built into. That is why this table
-- deliberately does NOT duplicate journal/line storage: a posting_execution
-- row is the record that a posting was ATTEMPTED and how it was resolved
-- (which account mappings, whose idempotency key, what the outcome was),
-- with journal_id pointing at the real committed journal once one exists.
-- The journal itself remains owned by CreateJournal/TransitionJournal, per
-- the spec's own "ledger entries are committed to ACC-05" line — ACC-04
-- orchestrates, it does not re-implement, the actual ledger write.
--
-- State model (verbatim): "Submitted → Validating → Ready → Committed or
-- Failed/Quarantined; no partial committed state." A normal mutable
-- stateful row, not append-only — same posture as this platform's other
-- run/batch tables (e.g. ACC-09's allocation_runs).
--
-- UNIQUE(tenant_id, source_event_id) WHERE source_event_id IS NOT NULL is
-- the spec's own negative path enforced at the database level: "Duplicate
-- source event" must return the prior result, never a second posting
-- consequence for the same source fact.

CREATE TABLE posting_executions (
    execution_id             UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL,
    legal_entity_id            UUID NOT NULL,
    kind                        VARCHAR(20) NOT NULL, -- EVENT | APPROVED_JOURNAL | REVERSAL
    source_event_id             VARCHAR(255), -- caller-declared idempotency anchor for PostAccountingEvent
    idempotency_key              VARCHAR(255),
    status                       VARCHAR(20) NOT NULL, -- SUBMITTED|VALIDATING|READY|COMMITTED|FAILED|QUARANTINED
    journal_id                   UUID, -- set once COMMITTED — the real ledger entry this execution produced
    calculation_trace             JSONB NOT NULL DEFAULT '{}'::jsonb, -- resolved account mappings, rounding notes — ExplainPosting's own data
    failure_reason                TEXT,
    correlation_id                 VARCHAR(255) NOT NULL,
    created_at                      TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id         VARCHAR(255) NOT NULL,
    committed_at                    TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX idx_posting_executions_source_event
    ON posting_executions (tenant_id, source_event_id)
    WHERE source_event_id IS NOT NULL;

ALTER TABLE posting_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE posting_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON posting_executions
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE INDEX idx_posting_executions_tenant_entity ON posting_executions (tenant_id, legal_entity_id);
CREATE INDEX idx_posting_executions_journal ON posting_executions (tenant_id, journal_id);
