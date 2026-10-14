-- Migration: 000800_ar08_cash_application.up.sql
-- AR-08 Cash Application (spec ZS-SVC-C-001 section 11). Invariants 8, 9, 10, 14; section 18 #22 unconfirmed payment applied,
-- #23 same receipt applied twice, #24 application does not reconcile to the receipt, #25 manual match above authority,
-- #26 application across a tenant / legal-entity boundary.
--   ar08_receipts          one per Banking-confirmed canonical transaction (unique per tenant); receipt = applied + unapplied + difference
--   ar08_batches           application batch (one currency); batch_receipts membership
--   ar08_candidates        append-only match candidates + scores (evidence)
--   ar08_manual_matches    manual / high-value / related-party proposals awaiting a different approver
--   ar08_applications      one ACTIVE (APPLIED) application per receipt; lines are append-only
--   ar08_reconciliations   append-only reconciliation evidence (no plug)

CREATE TABLE IF NOT EXISTS ar08_receipts (
    receipt_id          UUID PRIMARY KEY,
    tenant_id           UUID          NOT NULL,
    legal_entity_id     UUID          NOT NULL,
    bank_transaction_id UUID          NOT NULL,
    settlement_ref      VARCHAR(255)  NOT NULL CHECK (settlement_ref <> ''),
    customer_party_id   UUID,
    currency            CHAR(3)       NOT NULL,
    amount              NUMERIC(20,4) NOT NULL CONSTRAINT ar08_receipt_amount_positive CHECK (amount > 0),
    applied_amount      NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (applied_amount >= 0),
    unapplied_amount    NUMERIC(20,4) NOT NULL CHECK (unapplied_amount >= 0),
    difference_amount   NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (difference_amount >= 0),
    value_date          DATE          NOT NULL,
    payer_text          VARCHAR(500)  NOT NULL DEFAULT '',
    remittance_text     VARCHAR(500)  NOT NULL DEFAULT '',
    related_party       BOOLEAN       NOT NULL DEFAULT FALSE,
    state               VARCHAR(20)   NOT NULL DEFAULT 'UNAPPLIED'
        CONSTRAINT ar08_receipt_state CHECK (state IN ('UNAPPLIED','PARTIALLY_APPLIED','FULLY_APPLIED')),
    blocked             BOOLEAN       NOT NULL DEFAULT FALSE,
    control_exception   VARCHAR(500),
    registered_by       VARCHAR(255)  NOT NULL,
    version             INTEGER       NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar08_receipt_reconciles CHECK (amount = applied_amount + unapplied_amount + difference_amount),
    CONSTRAINT uq_ar08_receipt_bank_txn UNIQUE (tenant_id, bank_transaction_id)
);
CREATE INDEX IF NOT EXISTS idx_ar08_receipt_scope ON ar08_receipts (tenant_id, legal_entity_id, state);

CREATE TABLE IF NOT EXISTS ar08_batches (
    batch_id        UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    currency        CHAR(3)       NOT NULL,
    reference       VARCHAR(128)  NOT NULL DEFAULT '',
    state           VARCHAR(20)   NOT NULL DEFAULT 'UNAPPLIED'
        CONSTRAINT ar08_batch_state CHECK (state IN ('UNAPPLIED','PROPOSED','PARTIALLY_APPLIED','FULLY_APPLIED','RECONCILED')),
    version         INTEGER       NOT NULL DEFAULT 1,
    created_by      VARCHAR(255)  NOT NULL,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ar08_batch_receipts (
    tenant_id  UUID NOT NULL,
    batch_id   UUID NOT NULL REFERENCES ar08_batches(batch_id),
    receipt_id UUID NOT NULL REFERENCES ar08_receipts(receipt_id),
    added_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (batch_id, receipt_id),
    CONSTRAINT uq_ar08_receipt_one_batch UNIQUE (tenant_id, receipt_id)
);

CREATE TABLE IF NOT EXISTS ar08_candidates (
    candidate_id      UUID PRIMARY KEY,
    tenant_id         UUID          NOT NULL,
    legal_entity_id   UUID          NOT NULL,
    receipt_id        UUID          NOT NULL REFERENCES ar08_receipts(receipt_id),
    generation        INTEGER       NOT NULL,
    rank              INTEGER       NOT NULL,
    open_item_id      UUID          NOT NULL,
    customer_party_id UUID          NOT NULL,
    score             INTEGER       NOT NULL CHECK (score BETWEEN 0 AND 100),
    basis             JSONB         NOT NULL,
    residual_amount   NUMERIC(20,4) NOT NULL,
    suggested_amount  NUMERIC(20,4) NOT NULL CHECK (suggested_amount >= 0),
    disputed          BOOLEAN       NOT NULL DEFAULT FALSE,
    collection_hold   BOOLEAN       NOT NULL DEFAULT FALSE,
    generated_by      VARCHAR(255)  NOT NULL,
    generated_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar08_candidate UNIQUE (receipt_id, generation, open_item_id)
);

CREATE TABLE IF NOT EXISTS ar08_manual_matches (
    match_id          UUID PRIMARY KEY,
    tenant_id         UUID          NOT NULL,
    legal_entity_id   UUID          NOT NULL,
    receipt_id        UUID          NOT NULL REFERENCES ar08_receipts(receipt_id),
    batch_id          UUID          NOT NULL REFERENCES ar08_batches(batch_id),
    lines             JSONB         NOT NULL,
    difference_amount NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (difference_amount >= 0),
    difference_kind   VARCHAR(16)   NOT NULL DEFAULT '' CHECK (difference_kind IN ('','BANK_FEE','FX_DIFFERENCE')),
    reason_code       VARCHAR(64)   NOT NULL,
    evidence_id       VARCHAR(255)  NOT NULL DEFAULT '',
    requires          VARCHAR(255)  NOT NULL DEFAULT '',
    status            VARCHAR(12)   NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','APPLIED','REJECTED')),
    proposed_by       VARCHAR(255)  NOT NULL,
    approved_by       VARCHAR(255),
    application_id    UUID,
    proposed_at       TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    decided_at        TIMESTAMPTZ,
    CONSTRAINT ar08_match_decided CHECK ((status = 'PENDING' AND approved_by IS NULL) OR (status <> 'PENDING' AND approved_by IS NOT NULL)),
    CONSTRAINT ar08_match_sod CHECK (approved_by IS NULL OR approved_by <> proposed_by)
);

CREATE TABLE IF NOT EXISTS ar08_applications (
    application_id           UUID PRIMARY KEY,
    seq                      BIGSERIAL     NOT NULL,
    tenant_id                UUID          NOT NULL,
    legal_entity_id          UUID          NOT NULL,
    receipt_id               UUID          NOT NULL REFERENCES ar08_receipts(receipt_id),
    batch_id                 UUID          NOT NULL REFERENCES ar08_batches(batch_id),
    status                   VARCHAR(12)   NOT NULL DEFAULT 'APPLIED' CHECK (status IN ('APPLIED','UNAPPLIED')),
    mode                     VARCHAR(8)    NOT NULL CHECK (mode IN ('AUTO','MANUAL','REAPPLY')),
    request_hash             VARCHAR(64)   NOT NULL,
    currency                 CHAR(3)       NOT NULL,
    receipt_amount           NUMERIC(20,4) NOT NULL CHECK (receipt_amount > 0),
    applied_amount           NUMERIC(20,4) NOT NULL CHECK (applied_amount > 0),
    unapplied_amount         NUMERIC(20,4) NOT NULL CHECK (unapplied_amount >= 0),
    difference_amount        NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (difference_amount >= 0),
    difference_kind          VARCHAR(16)   NOT NULL DEFAULT '' CHECK (difference_kind IN ('','BANK_FEE','FX_DIFFERENCE')),
    reason_code              VARCHAR(64),
    evidence_id              VARCHAR(255),
    match_id                 UUID,
    reapplies_application_id UUID,
    applied_by               VARCHAR(255)  NOT NULL,
    approved_by              VARCHAR(255),
    applied_at               TIMESTAMPTZ   NOT NULL,
    unapplied_by             VARCHAR(255),
    unapplied_at             TIMESTAMPTZ,
    unapply_reason_code      VARCHAR(64),
    unapply_evidence_id      VARCHAR(255),
    accounting_state         VARCHAR(12)   NOT NULL DEFAULT 'PENDING' CHECK (accounting_state IN ('PENDING','POSTED','FAILED','QUARANTINED')),
    journal_id               VARCHAR(128),
    accounting_error         VARCHAR(1000),
    unapply_accounting_state VARCHAR(12) CHECK (unapply_accounting_state IN ('PENDING','POSTED','FAILED','QUARANTINED')),
    unapply_journal_id       VARCHAR(128),
    unapply_accounting_error VARCHAR(1000),
    version                  INTEGER       NOT NULL DEFAULT 1,
    CONSTRAINT ar08_app_reconciles CHECK (receipt_amount = applied_amount + unapplied_amount + difference_amount),
    CONSTRAINT ar08_app_unapplied_consistent CHECK ((status = 'APPLIED' AND unapplied_by IS NULL) OR (status = 'UNAPPLIED' AND unapplied_by IS NOT NULL AND unapply_reason_code IS NOT NULL))
);
-- At most one ACTIVE application per receipt: the structural guard behind "same receipt applied twice" (#23).
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar08_one_active_application ON ar08_applications (tenant_id, receipt_id) WHERE status = 'APPLIED';
CREATE INDEX IF NOT EXISTS idx_ar08_app_batch ON ar08_applications (tenant_id, batch_id);

CREATE TABLE IF NOT EXISTS ar08_application_lines (
    line_id        UUID PRIMARY KEY,
    tenant_id      UUID          NOT NULL,
    application_id UUID          NOT NULL REFERENCES ar08_applications(application_id),
    line_no        INTEGER       NOT NULL CHECK (line_no > 0),
    open_item_id   UUID          NOT NULL,
    amount         NUMERIC(20,4) NOT NULL CONSTRAINT ar08_line_positive CHECK (amount > 0),
    CONSTRAINT uq_ar08_line_no UNIQUE (application_id, line_no),
    CONSTRAINT uq_ar08_line_item UNIQUE (application_id, open_item_id)
);

CREATE TABLE IF NOT EXISTS ar08_reconciliations (
    reconciliation_id UUID PRIMARY KEY,
    tenant_id         UUID          NOT NULL,
    legal_entity_id   UUID          NOT NULL,
    batch_id          UUID          NOT NULL REFERENCES ar08_batches(batch_id),
    currency          CHAR(3)       NOT NULL,
    status            VARCHAR(12)   NOT NULL CHECK (status IN ('RECONCILED','EXCEPTION')),
    receipts_total    NUMERIC(20,4) NOT NULL,
    applied_total     NUMERIC(20,4) NOT NULL,
    unapplied_total   NUMERIC(20,4) NOT NULL,
    difference_total  NUMERIC(20,4) NOT NULL,
    findings          JSONB         NOT NULL,
    performed_by      VARCHAR(255)  NOT NULL,
    performed_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar08_recon_balanced CHECK (status <> 'RECONCILED' OR receipts_total = applied_total + unapplied_total + difference_total)
);

-- Append-only evidence.
DROP TRIGGER IF EXISTS ar08_lines_append_only ON ar08_application_lines;
CREATE TRIGGER ar08_lines_append_only BEFORE UPDATE OR DELETE ON ar08_application_lines FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();
DROP TRIGGER IF EXISTS ar08_candidates_append_only ON ar08_candidates;
CREATE TRIGGER ar08_candidates_append_only BEFORE UPDATE OR DELETE ON ar08_candidates FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();
DROP TRIGGER IF EXISTS ar08_recon_append_only ON ar08_reconciliations;
CREATE TRIGGER ar08_recon_append_only BEFORE UPDATE OR DELETE ON ar08_reconciliations FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- The settled facts of a receipt (what Banking confirmed) never change; only the application dimension moves.
CREATE OR REPLACE FUNCTION ar08_guard_receipt() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR08_RECEIPT_NOT_DELETABLE: a receipt is never deleted';
    END IF;
    IF NEW.receipt_id <> OLD.receipt_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.bank_transaction_id <> OLD.bank_transaction_id OR NEW.settlement_ref <> OLD.settlement_ref
       OR NEW.currency <> OLD.currency OR NEW.amount <> OLD.amount OR NEW.value_date <> OLD.value_date
       OR NEW.registered_by <> OLD.registered_by OR NEW.related_party <> OLD.related_party THEN
        RAISE EXCEPTION 'AR08_RECEIPT_IMMUTABLE: the confirmed settlement facts of a receipt never change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar08_receipt_guard ON ar08_receipts;
CREATE TRIGGER ar08_receipt_guard BEFORE UPDATE OR DELETE ON ar08_receipts FOR EACH ROW EXECUTE FUNCTION ar08_guard_receipt();

-- An application's amounts, receipt and lines are frozen; only its reversal / accounting mirror fields move (reversal is
-- one-way: APPLIED -> UNAPPLIED).
CREATE OR REPLACE FUNCTION ar08_guard_application() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR08_APPLICATION_NOT_DELETABLE: an application is reversed by unapply, never deleted';
    END IF;
    IF NEW.application_id <> OLD.application_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.receipt_id <> OLD.receipt_id OR NEW.batch_id <> OLD.batch_id OR NEW.mode <> OLD.mode OR NEW.request_hash <> OLD.request_hash
       OR NEW.currency <> OLD.currency OR NEW.receipt_amount <> OLD.receipt_amount OR NEW.applied_amount <> OLD.applied_amount
       OR NEW.unapplied_amount <> OLD.unapplied_amount OR NEW.difference_amount <> OLD.difference_amount
       OR NEW.difference_kind <> OLD.difference_kind OR NEW.applied_by <> OLD.applied_by OR NEW.applied_at <> OLD.applied_at THEN
        RAISE EXCEPTION 'AR08_APPLICATION_IMMUTABLE: an application is never edited; unapply and reapply';
    END IF;
    IF OLD.status = 'UNAPPLIED' AND NEW.status <> 'UNAPPLIED' THEN
        RAISE EXCEPTION 'AR08_APPLICATION_REVERSED: an unapplied application cannot be reinstated; reapply creates a new one';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar08_application_guard ON ar08_applications;
CREATE TRIGGER ar08_application_guard BEFORE UPDATE OR DELETE ON ar08_applications FOR EACH ROW EXECUTE FUNCTION ar08_guard_application();

-- A manual match's content is frozen once proposed; only its decision fields move, once.
CREATE OR REPLACE FUNCTION ar08_guard_match() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR08_MATCH_NOT_DELETABLE: a manual match is never deleted';
    END IF;
    IF NEW.match_id <> OLD.match_id OR NEW.receipt_id <> OLD.receipt_id OR NEW.batch_id <> OLD.batch_id OR NEW.lines::text <> OLD.lines::text
       OR NEW.difference_amount <> OLD.difference_amount OR NEW.proposed_by <> OLD.proposed_by OR NEW.reason_code <> OLD.reason_code THEN
        RAISE EXCEPTION 'AR08_MATCH_IMMUTABLE: the proposed content of a manual match never changes';
    END IF;
    IF OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'AR08_MATCH_DECIDED: a decided manual match cannot change';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar08_match_guard ON ar08_manual_matches;
CREATE TRIGGER ar08_match_guard BEFORE UPDATE OR DELETE ON ar08_manual_matches FOR EACH ROW EXECUTE FUNCTION ar08_guard_match();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar08_receipts','ar08_batches','ar08_batch_receipts','ar08_candidates','ar08_manual_matches','ar08_applications','ar08_application_lines','ar08_reconciliations'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
