-- ZS-SVC-D-001 AP-11.
--
-- 1. Transactional outbox (ZS-STATE-001 I-13): domain events are written in
--    the same transaction as the state change and relayed to Kafka, instead
--    of being published best-effort after commit with the error ignored.
CREATE TABLE outbox_events (
    outbox_event_id  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type   VARCHAR(64) NOT NULL,
    aggregate_id     VARCHAR(255) NOT NULL,
    event_type       VARCHAR(128) NOT NULL,
    tenant_id        UUID NULL,
    legal_entity_id  UUID NOT NULL,
    actor_id         VARCHAR(255) NULL,
    correlation_id   VARCHAR(255) NULL,
    headers          JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload          JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at     TIMESTAMPTZ NULL,
    publish_attempts INT NOT NULL DEFAULT 0,
    last_error       TEXT NULL
);
CREATE INDEX idx_outbox_events_unpublished ON outbox_events (created_at ASC) WHERE published_at IS NULL;
CREATE INDEX idx_outbox_events_tenant ON outbox_events (tenant_id);
-- Internal queue polled across tenants by the relay: no RLS (same convention
-- as the other services' outbox tables).

-- 2. Payment-settlement accounting (spec §19: "Payment settled -> payable
--    settlement + bank/payment-clearing accounting consequence"). AP never
--    writes the ledger directly: when BNK-07 settles an instruction, a
--    posting request is recorded in the SAME transaction and a dispatcher
--    submits it to ACC-04 (general-ledger-svc POST /v1/postings/events),
--    which is idempotent on source_event_id. The status here is what
--    GetAccountingStatus reports.
CREATE TABLE accounting_posting_requests (
    request_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NULL,
    legal_entity_id      UUID NOT NULL,
    run_id               UUID NOT NULL REFERENCES payment_runs (run_id),
    instruction_id       UUID NOT NULL REFERENCES run_instructions (instruction_id),
    source_event_id      TEXT NOT NULL,
    request_payload      JSONB NOT NULL,
    status               TEXT NOT NULL DEFAULT 'PENDING'
                         CHECK (status IN ('PENDING', 'POSTED', 'FAILED', 'QUARANTINED')),
    attempts             INT NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    posting_execution_id TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one posting request per source event: a replayed settlement can
-- never request a second posting.
CREATE UNIQUE INDEX uq_accounting_posting_requests_source
    ON accounting_posting_requests (COALESCE(tenant_id::text, ''), source_event_id);
CREATE INDEX idx_accounting_posting_requests_pending
    ON accounting_posting_requests (created_at) WHERE status = 'PENDING';
CREATE INDEX idx_accounting_posting_requests_run ON accounting_posting_requests (run_id);

-- The request itself is evidence and never changes; only the dispatch
-- bookkeeping columns may.
CREATE OR REPLACE FUNCTION reject_accounting_request_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting_posting_requests rows are never deleted';
    END IF;
    IF OLD.status = 'POSTED' THEN
        RAISE EXCEPTION 'accounting posting request % is POSTED and final', OLD.request_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.instruction_id IS DISTINCT FROM OLD.instruction_id
        OR NEW.source_event_id IS DISTINCT FROM OLD.source_event_id
        OR NEW.request_payload IS DISTINCT FROM OLD.request_payload
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'accounting posting request % may only change its dispatch status', OLD.request_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_accounting_request_mutation
    BEFORE UPDATE OR DELETE ON accounting_posting_requests
    FOR EACH ROW EXECUTE FUNCTION reject_accounting_request_mutation();

ALTER TABLE accounting_posting_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounting_posting_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON accounting_posting_requests
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
