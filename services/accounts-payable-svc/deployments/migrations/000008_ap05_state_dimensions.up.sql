-- Migration: 000008_ap05_state_dimensions.up.sql
--
-- AP-05 (ZS-SVC-D-001 section 8): orthogonal state dimensions, immutable source
-- representation, duplicate assessment, tax/withholding provenance, invoice
-- bank-detail evidence, append-only history, correction links, command
-- idempotency and the durable AP-08 payable-creation queue.
--
-- `status` is kept and stays populated as a DERIVED summary of the dimensions so
-- existing readers (financial-close-svc, treasury-svc, invoice-approval-svc) keep
-- working. It is never the source of truth for a transition any more.

BEGIN;

ALTER TABLE vendor_invoices
    ADD COLUMN IF NOT EXISTS version                    INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS document_type              VARCHAR(16) NOT NULL DEFAULT 'INVOICE',

    -- Orthogonal dimensions (Figure 7).
    ADD COLUMN IF NOT EXISTS intake_state               VARCHAR(16) NOT NULL DEFAULT 'RECEIVED',
    ADD COLUMN IF NOT EXISTS match_state                VARCHAR(16) NOT NULL DEFAULT 'NOT_MATCHED',
    ADD COLUMN IF NOT EXISTS approval_state             VARCHAR(16) NOT NULL DEFAULT 'NONE',
    ADD COLUMN IF NOT EXISTS accounting_state           VARCHAR(16) NOT NULL DEFAULT 'NOT_REQUESTED',
    ADD COLUMN IF NOT EXISTS settlement_state           VARCHAR(20) NOT NULL DEFAULT 'UNSETTLED',
    ADD COLUMN IF NOT EXISTS hold_state                 VARCHAR(16) NOT NULL DEFAULT 'NONE',
    ADD COLUMN IF NOT EXISTS hold_reason                TEXT,

    -- Immutable source representation.
    ADD COLUMN IF NOT EXISTS source_hash                CHAR(64),
    ADD COLUMN IF NOT EXISTS source_payload             JSONB,
    ADD COLUMN IF NOT EXISTS attachment_hash            CHAR(64),
    ADD COLUMN IF NOT EXISTS source_channel             VARCHAR(64),
    ADD COLUMN IF NOT EXISTS source_accepted_at         TIMESTAMPTZ,

    -- Duplicate handling.
    ADD COLUMN IF NOT EXISTS invoice_number_normalized  VARCHAR(255),
    ADD COLUMN IF NOT EXISTS duplicate_state            VARCHAR(16) NOT NULL DEFAULT 'CLEAR',
    ADD COLUMN IF NOT EXISTS quarantine_reason          TEXT,

    -- Lifecycle actors.
    ADD COLUMN IF NOT EXISTS submitted_by_principal_id  VARCHAR(255),
    ADD COLUMN IF NOT EXISTS submitted_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rejected_by_principal_id   VARCHAR(255),
    ADD COLUMN IF NOT EXISTS rejected_at                TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reject_reason              TEXT,

    -- TAX / withholding provenance (never calculated here).
    ADD COLUMN IF NOT EXISTS tax_state                  VARCHAR(16) NOT NULL DEFAULT 'NOT_VERIFIED',
    ADD COLUMN IF NOT EXISTS tax_provenance             JSONB,
    ADD COLUMN IF NOT EXISTS tax_result_hash            VARCHAR(64),
    ADD COLUMN IF NOT EXISTS tax_verified_at            TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS withholding_ref            TEXT,
    ADD COLUMN IF NOT EXISTS withholding_determination_id TEXT,
    ADD COLUMN IF NOT EXISTS withholding_provenance     JSONB,

    -- Invoice-supplied bank data: EVIDENCE ONLY, never a payment destination.
    ADD COLUMN IF NOT EXISTS extracted_bank_details     JSONB,
    ADD COLUMN IF NOT EXISTS payee_state                VARCHAR(16) NOT NULL DEFAULT 'NOT_PROVIDED',
    ADD COLUMN IF NOT EXISTS payee_check                JSONB,

    -- Server-resolved context captured at validation.
    ADD COLUMN IF NOT EXISTS supplier_profile_id        TEXT,
    ADD COLUMN IF NOT EXISTS supplier_profile_version   INTEGER,
    ADD COLUMN IF NOT EXISTS po_revision                INTEGER,

    -- AP-06 gate. match_required is decided at validation (PO-backed invoice);
    -- match_cleared is written only by the matching module.
    ADD COLUMN IF NOT EXISTS match_required             BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS match_cleared              BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS match_run_id               UUID,

    -- Downstream lineage.
    ADD COLUMN IF NOT EXISTS payable_id                 TEXT,
    ADD COLUMN IF NOT EXISTS accounting_event_id        UUID;

-- Backfill the dimensions from the legacy single status.
UPDATE vendor_invoices SET
    intake_state     = CASE status WHEN 'RECEIVED' THEN 'RECEIVED' ELSE 'VALIDATED' END,
    approval_state   = CASE WHEN status IN ('APPROVED', 'PAYMENT_REQUESTED') THEN 'APPROVED' ELSE 'NONE' END,
    accounting_state = CASE WHEN status IN ('APPROVED', 'PAYMENT_REQUESTED') THEN 'REQUESTED' ELSE 'NOT_REQUESTED' END,
    source_accepted_at = CASE WHEN status = 'RECEIVED' THEN NULL ELSE COALESCE(validated_at, created_at) END,
    invoice_number_normalized = regexp_replace(
        regexp_replace(lower(invoice_number), '(^|[^0-9])0+([0-9])', '\1\2', 'g'),
        '[^a-z0-9]', '', 'g'),
    source_channel   = 'legacy-backfill',
    source_hash      = encode(sha256(convert_to(
        invoice_id::text || '|' || vendor_id || '|' || invoice_number || '|' || amount::text || '|' || currency_code,
        'UTF8')), 'hex')
WHERE source_hash IS NULL;

ALTER TABLE vendor_invoices
    ALTER COLUMN source_hash SET NOT NULL,
    ALTER COLUMN source_channel SET NOT NULL,
    ALTER COLUMN invoice_number_normalized SET NOT NULL;

ALTER TABLE vendor_invoices
    ADD CONSTRAINT chk_vi_intake     CHECK (intake_state IN ('RECEIVED','VALIDATED','QUARANTINED','REJECTED')),
    ADD CONSTRAINT chk_vi_match      CHECK (match_state IN ('NOT_MATCHED','MATCHED','WITHIN_TOLERANCE','EXCEPTION','INCOMPLETE')),
    ADD CONSTRAINT chk_vi_approval   CHECK (approval_state IN ('NONE','PENDING','APPROVED','REJECTED')),
    ADD CONSTRAINT chk_vi_accounting CHECK (accounting_state IN ('NOT_REQUESTED','REQUESTED','POSTED','FAILED')),
    ADD CONSTRAINT chk_vi_settlement CHECK (settlement_state IN ('UNSETTLED','PARTIALLY_SETTLED','SETTLED')),
    ADD CONSTRAINT chk_vi_hold       CHECK (hold_state IN ('NONE','HELD','DISPUTED')),
    ADD CONSTRAINT chk_vi_doctype    CHECK (document_type IN ('INVOICE','CREDIT_NOTE','DEBIT_NOTE')),
    ADD CONSTRAINT chk_vi_dup        CHECK (duplicate_state IN ('CLEAR','SUSPECTED','CLEARED','CONFIRMED')),
    ADD CONSTRAINT chk_vi_tax        CHECK (tax_state IN ('NOT_VERIFIED','VERIFIED','CHANGED')),
    ADD CONSTRAINT chk_vi_payee      CHECK (payee_state IN ('NOT_PROVIDED','MATCHES_ORG10','MISMATCH','RESOLVED'));

CREATE INDEX IF NOT EXISTS idx_vendor_invoices_near_dup
    ON vendor_invoices (tenant_id, vendor_id, invoice_number_normalized);
CREATE INDEX IF NOT EXISTS idx_vendor_invoices_amount_dup
    ON vendor_invoices (tenant_id, vendor_id, currency_code, amount);

-- ── Immutability ───────────────────────────────────────────────────────────
-- Once a supplier invoice has been accepted (source_accepted_at set) its source
-- representation can never change; a correction is a linked credit/debit
-- document or a governed reversal. The runtime is a Postgres superuser, so a
-- trigger -- not a GRANT -- is the real enforcement.
CREATE OR REPLACE FUNCTION vendor_invoices_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'vendor_invoices rows are never deleted (no soft-delete doctrine)'
            USING ERRCODE = '23000';
    END IF;
    IF OLD.source_accepted_at IS NOT NULL AND (
           NEW.source_accepted_at IS DISTINCT FROM OLD.source_accepted_at
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.vendor_id IS DISTINCT FROM OLD.vendor_id
        OR NEW.invoice_number IS DISTINCT FROM OLD.invoice_number
        OR NEW.invoice_number_normalized IS DISTINCT FROM OLD.invoice_number_normalized
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency_code IS DISTINCT FROM OLD.currency_code
        OR NEW.due_date IS DISTINCT FROM OLD.due_date
        OR NEW.invoice_date IS DISTINCT FROM OLD.invoice_date
        OR NEW.supply_date IS DISTINCT FROM OLD.supply_date
        OR NEW.net_amount IS DISTINCT FROM OLD.net_amount
        OR NEW.tax_amount IS DISTINCT FROM OLD.tax_amount
        OR NEW.purchase_order_id IS DISTINCT FROM OLD.purchase_order_id
        OR NEW.goods_receipt_ref IS DISTINCT FROM OLD.goods_receipt_ref
        OR NEW.invoice_document_id IS DISTINCT FROM OLD.invoice_document_id
        OR NEW.document_type IS DISTINCT FROM OLD.document_type
        OR NEW.source_hash IS DISTINCT FROM OLD.source_hash
        OR NEW.source_payload IS DISTINCT FROM OLD.source_payload
        OR NEW.attachment_hash IS DISTINCT FROM OLD.attachment_hash
        OR NEW.source_channel IS DISTINCT FROM OLD.source_channel
        OR NEW.extracted_bank_details IS DISTINCT FROM OLD.extracted_bank_details
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    ) THEN
        RAISE EXCEPTION 'accepted supplier invoice source representation is immutable; use a linked credit/debit document'
            USING ERRCODE = '23000';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_vendor_invoices_guard ON vendor_invoices;
CREATE TRIGGER trg_vendor_invoices_guard
    BEFORE UPDATE OR DELETE ON vendor_invoices
    FOR EACH ROW EXECUTE FUNCTION vendor_invoices_guard();

CREATE OR REPLACE FUNCTION vendor_invoice_lines_guard() RETURNS trigger AS $$
DECLARE accepted TIMESTAMPTZ;
BEGIN
    SELECT source_accepted_at INTO accepted FROM vendor_invoices
     WHERE invoice_id = COALESCE(OLD.invoice_id, NEW.invoice_id);
    IF accepted IS NOT NULL THEN
        RAISE EXCEPTION 'lines of an accepted supplier invoice are immutable'
            USING ERRCODE = '23000';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_vendor_invoice_lines_guard ON vendor_invoice_lines;
CREATE TRIGGER trg_vendor_invoice_lines_guard
    BEFORE UPDATE OR DELETE ON vendor_invoice_lines
    FOR EACH ROW EXECUTE FUNCTION vendor_invoice_lines_guard();

-- Generic append-only guard.
CREATE OR REPLACE FUNCTION ap_append_only_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '23000';
END;
$$ LANGUAGE plpgsql;

-- ── Duplicate assessments (persisted, reproducible) ────────────────────────
CREATE TABLE IF NOT EXISTS invoice_duplicate_assessments (
    assessment_id       UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    invoice_id          UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    verdict             VARCHAR(16) NOT NULL CHECK (verdict IN ('CLEAR','NEAR_DUPLICATE')),
    score               NUMERIC(5,4) NOT NULL,
    exact_key           TEXT NOT NULL,
    near_key            TEXT NOT NULL,
    inputs              JSONB NOT NULL,
    config              JSONB NOT NULL,
    matched_invoice_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    matches             JSONB NOT NULL DEFAULT '[]'::jsonb,
    trigger_command     VARCHAR(64) NOT NULL,
    assessed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_dup_assess_invoice ON invoice_duplicate_assessments (tenant_id, invoice_id, assessed_at DESC);
DROP TRIGGER IF EXISTS trg_dup_assess_append_only ON invoice_duplicate_assessments;
CREATE TRIGGER trg_dup_assess_append_only BEFORE UPDATE OR DELETE ON invoice_duplicate_assessments
    FOR EACH ROW EXECUTE FUNCTION ap_append_only_guard();

-- ── Append-only history ────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS invoice_history (
    history_id     BIGSERIAL PRIMARY KEY,
    tenant_id      UUID NOT NULL,
    invoice_id     UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    version        INTEGER NOT NULL,
    command        VARCHAR(64) NOT NULL,
    actor_id       VARCHAR(255) NOT NULL,
    reason         TEXT,
    from_state     JSONB,
    to_state       JSONB,
    detail         JSONB,
    correlation_id VARCHAR(255),
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_invoice_history_invoice ON invoice_history (tenant_id, invoice_id, history_id);
DROP TRIGGER IF EXISTS trg_invoice_history_append_only ON invoice_history;
CREATE TRIGGER trg_invoice_history_append_only BEFORE UPDATE OR DELETE ON invoice_history
    FOR EACH ROW EXECUTE FUNCTION ap_append_only_guard();

-- ── Correction links (credit/debit/reversal/replacement) ───────────────────
CREATE TABLE IF NOT EXISTS invoice_correction_links (
    link_id             UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    original_invoice_id UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    linked_invoice_id   UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    link_kind           VARCHAR(16) NOT NULL CHECK (link_kind IN ('CREDIT','DEBIT','REVERSAL','REPLACEMENT')),
    reason              TEXT NOT NULL,
    created_by          VARCHAR(255) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (original_invoice_id <> linked_invoice_id),
    UNIQUE (tenant_id, original_invoice_id, linked_invoice_id)
);
DROP TRIGGER IF EXISTS trg_correction_links_append_only ON invoice_correction_links;
CREATE TRIGGER trg_correction_links_append_only BEFORE UPDATE OR DELETE ON invoice_correction_links
    FOR EACH ROW EXECUTE FUNCTION ap_append_only_guard();

-- ── Command idempotency (spec section 16) ──────────────────────────────────
-- response IS NULL means "claimed, still executing". A claim older than the
-- stale window may be taken over by a retry.
CREATE TABLE IF NOT EXISTS ap_command_idempotency (
    tenant_id       UUID NOT NULL,
    idempotency_key TEXT NOT NULL,
    operation       TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    status_code     INTEGER,
    response        JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, idempotency_key)
);

-- ── Durable AP-08 payable-creation queue ───────────────────────────────────
-- Approval must create the payable reliably. A row is written in the SAME
-- transaction as the approval; a relay retries until AP-08 accepts (idempotent
-- on source_reference). Infrastructure queue read by a cross-tenant worker, so
-- -- like outbox_events -- it carries no RLS.
CREATE TABLE IF NOT EXISTS payable_creation_requests (
    request_id       UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    legal_entity_id  UUID NOT NULL,
    invoice_id       UUID NOT NULL REFERENCES vendor_invoices(invoice_id),
    source_reference TEXT NOT NULL,
    payload          JSONB NOT NULL,
    principal_id     VARCHAR(255) NOT NULL,
    correlation_id   VARCHAR(255) NOT NULL,
    status           VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                     CHECK (status IN ('PENDING','IN_PROGRESS','CREATED','BLOCKED','DEAD')),
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_until     TIMESTAMPTZ,
    last_error       TEXT,
    payable_id       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at     TIMESTAMPTZ,
    UNIQUE (tenant_id, invoice_id)
);
CREATE INDEX IF NOT EXISTS idx_payable_requests_due ON payable_creation_requests (next_attempt_at)
    WHERE status IN ('PENDING','IN_PROGRESS');

-- ── RLS for the tenant-owned tables (no `IS NULL OR` escape clause) ─────────
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['invoice_duplicate_assessments','invoice_history','invoice_correction_links','ap_command_idempotency']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;

COMMIT;
