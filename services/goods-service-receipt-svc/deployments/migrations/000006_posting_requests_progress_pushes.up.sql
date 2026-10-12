-- Migration: 000006_posting_requests_progress_pushes.up.sql
--
--   * accounting_posting_requests - the durable GRNI posting queue. Confirming
--     (or reversing) a receipt inserts one row in the SAME transaction; a
--     dispatcher posts it to general-ledger-svc's ACC-04 endpoint
--     (POST /v1/postings/events, idempotent on source_event_id) and records the
--     outcome. UNIQUE (tenant_id, source_event_id) plus ON CONFLICT DO NOTHING
--     means a replay can never queue - or post - the same consequence twice.
--     This table is the source of truth for GetReceiptAccountingStatus; the old
--     receipt_accounting_events rows (POSTED/EXCEPTION from the retired raw-
--     journal path) stay readable as history.
--   * po_progress_pushes - durable queue of received-quantity deltas owed to
--     AP-03 (purchase-order-svc), drained by a background worker.
--
-- Both are infrastructure queues drained across tenants by their workers, so
-- like outbox_events they carry no RLS; every handler query names tenant_id.

CREATE TABLE accounting_posting_requests (
    request_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL,
    legal_entity_id        UUID NOT NULL,
    receipt_id             UUID NOT NULL REFERENCES goods_service_receipts (receipt_id),
    -- receipt id for the accrual; '<receipt id>:reversal:<reversal id>' for a reversal.
    source_event_id        TEXT NOT NULL,
    direction              TEXT NOT NULL CHECK (direction IN ('ACCRUE', 'REVERSE')),
    posting_policy_version TEXT NOT NULL,
    request_payload        JSONB NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'PENDING'
                            CHECK (status IN ('PENDING', 'POSTED', 'FAILED', 'QUARANTINED')),
    attempts               INT NOT NULL DEFAULT 0,
    last_error             TEXT NULL,
    posting_execution_id   TEXT NULL,
    journal_id             TEXT NULL,
    correlation_id         TEXT NULL,
    actor_id               TEXT NULL,
    next_attempt_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    posted_at              TIMESTAMPTZ NULL,
    CONSTRAINT uq_accounting_posting_requests_source UNIQUE (tenant_id, source_event_id)
);
CREATE INDEX idx_accounting_posting_requests_due ON accounting_posting_requests (next_attempt_at, created_at) WHERE status = 'PENDING';
CREATE INDEX idx_accounting_posting_requests_receipt ON accounting_posting_requests (tenant_id, receipt_id, created_at DESC);

-- The request itself is evidence: only delivery bookkeeping may change, and a
-- row is never deleted.
CREATE OR REPLACE FUNCTION guard_posting_request() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounting_posting_requests rows are never deleted';
    END IF;
    IF NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.receipt_id IS DISTINCT FROM OLD.receipt_id
        OR NEW.source_event_id IS DISTINCT FROM OLD.source_event_id
        OR NEW.direction IS DISTINCT FROM OLD.direction
        OR NEW.request_payload IS DISTINCT FROM OLD.request_payload
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'accounting_posting_requests: the request itself is immutable';
    END IF;
    IF OLD.status IN ('POSTED') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'accounting_posting_requests: a POSTED request cannot change status';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_guard_posting_request
    BEFORE UPDATE OR DELETE ON accounting_posting_requests
    FOR EACH ROW EXECUTE FUNCTION guard_posting_request();

CREATE TABLE po_progress_pushes (
    push_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    legal_entity_id   UUID NOT NULL,
    receipt_id        UUID NOT NULL REFERENCES goods_service_receipts (receipt_id),
    purchase_order_id UUID NOT NULL,
    po_line_id        UUID NOT NULL,
    kind              TEXT NOT NULL DEFAULT 'RECEIVED' CHECK (kind IN ('RECEIVED')),
    quantity          NUMERIC(18, 4) NOT NULL CHECK (quantity > 0),
    amount            NUMERIC(18, 2) NOT NULL CHECK (amount > 0),
    delta_sign        SMALLINT NOT NULL CHECK (delta_sign IN (1, -1)),
    -- receipt id for the original push, reversal id for a reversal push.
    source_ref        TEXT NOT NULL,
    correlation_id    TEXT NULL,
    status            TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'DELIVERED', 'FAILED')),
    attempts          INT NOT NULL DEFAULT 0,
    last_error        TEXT NULL,
    next_attempt_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at      TIMESTAMPTZ NULL,
    CONSTRAINT uq_po_progress_pushes_source UNIQUE (tenant_id, kind, source_ref)
);
CREATE INDEX idx_po_progress_pushes_due ON po_progress_pushes (next_attempt_at, created_at) WHERE status = 'PENDING';
CREATE INDEX idx_po_progress_pushes_receipt ON po_progress_pushes (tenant_id, receipt_id);
CREATE INDEX idx_po_progress_pushes_line ON po_progress_pushes (tenant_id, purchase_order_id, po_line_id);
