-- AP-07 to ACC-04 hand-off. The approval transaction inserts one row here;
-- the accounting dispatcher POSTs it to general-ledger-svc
-- /v1/postings/events (idempotent on source_event_id) and records the real
-- outcome. No service writes the ledger directly.
CREATE TABLE accounting_posting_requests (
    request_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    legal_entity_id       UUID NOT NULL,
    aggregate_id          TEXT NOT NULL,
    source_event_id       TEXT NOT NULL,
    request_payload       JSONB NOT NULL,
    status                TEXT NOT NULL DEFAULT 'PENDING'
                          CHECK (status IN ('PENDING', 'POSTED', 'FAILED', 'QUARANTINED')),
    attempts              INT  NOT NULL DEFAULT 0,
    last_error            TEXT NOT NULL DEFAULT '',
    posting_execution_id  TEXT NOT NULL DEFAULT '',
    journal_id            TEXT NOT NULL DEFAULT '',
    next_attempt_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at             TIMESTAMPTZ NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_accounting_posting_source UNIQUE (tenant_id, source_event_id)
);
CREATE INDEX idx_accounting_posting_due ON accounting_posting_requests (next_attempt_at) WHERE status = 'PENDING';
CREATE INDEX idx_accounting_posting_aggregate ON accounting_posting_requests (aggregate_id);

CREATE OR REPLACE FUNCTION reject_accounting_posting_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting_posting_requests rows are never deleted';
    END IF;
    IF OLD.status IN ('POSTED', 'FAILED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'accounting posting request % is final (%)', OLD.request_id, OLD.status;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
        OR NEW.source_event_id IS DISTINCT FROM OLD.source_event_id
        OR NEW.request_payload IS DISTINCT FROM OLD.request_payload
    THEN
        RAISE EXCEPTION 'accounting posting request % basis can never change', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_accounting_posting_mutation
    BEFORE UPDATE OR DELETE ON accounting_posting_requests
    FOR EACH ROW EXECUTE FUNCTION reject_accounting_posting_mutation();

ALTER TABLE accounting_posting_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_posting_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounting_posting_requests
    USING (
        tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.system_relay', true), ''), 'false') = 'true'
    );
