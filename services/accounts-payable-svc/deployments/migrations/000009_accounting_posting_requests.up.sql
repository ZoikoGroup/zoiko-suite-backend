-- Migration: 000009_accounting_posting_requests.up.sql
--
-- Durable ACC-04 posting queue. Approval (or credit-document acceptance) writes
-- a row here in the SAME transaction as the state change; a dispatcher POSTs
-- PENDING rows to general-ledger-svc /v1/postings/events, idempotent on
-- source_event_id (= the invoice / credit document id). The invoice's
-- accounting_state mirrors this table: REQUESTED -> POSTED | FAILED.
-- Like outbox_events and payable_creation_requests it is an infrastructure
-- queue read by a cross-tenant worker, so it carries no RLS.

CREATE TABLE IF NOT EXISTS accounting_posting_requests (
    request_id           UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL,
    legal_entity_id      UUID NOT NULL,
    invoice_id           UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    source_event_id      TEXT NOT NULL,
    request_payload      JSONB NOT NULL,
    principal_id         VARCHAR(255) NOT NULL,
    correlation_id       VARCHAR(255) NOT NULL,
    status               VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                         CHECK (status IN ('PENDING','IN_PROGRESS','POSTED','FAILED','QUARANTINED')),
    attempts             INTEGER NOT NULL DEFAULT 0,
    next_attempt_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_until         TIMESTAMPTZ,
    last_error           TEXT,
    posting_execution_id TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at         TIMESTAMPTZ,
    UNIQUE (tenant_id, source_event_id)
);
CREATE INDEX IF NOT EXISTS idx_acc_posting_due ON accounting_posting_requests (next_attempt_at)
    WHERE status IN ('PENDING','IN_PROGRESS');
