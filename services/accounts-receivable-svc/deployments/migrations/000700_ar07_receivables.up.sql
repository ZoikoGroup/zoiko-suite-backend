-- Migration: 000700_ar07_receivables.up.sql
-- AR-07 Receivables (spec ZS-SVC-C-001 section 10). Customer open items, the append-only adjustment ledger that is the ONLY
-- way a residual moves (invariants 6, 7, 15, 17; section 18 #19 negative residual, #20 manual balance overwrite, #21 duplicate
-- or missing open items).
--   ar07_open_items      one per source document (unique), residual CHECKed 0..original, dispute / hold flags orthogonal
--   ar07_adjustments     append-only ledger: signed effect, owning event ref, event_key UNIQUE per tenant, balance chain
--   ar07_disputes        dispute lifecycle (one OPEN dispute per item), opener / resolver for SoD
--   ar07_item_history    append-only evidence of every state change

CREATE TABLE IF NOT EXISTS ar07_open_items (
    open_item_id        UUID PRIMARY KEY,
    tenant_id           UUID          NOT NULL,
    legal_entity_id     UUID          NOT NULL,
    document_kind       VARCHAR(16)   NOT NULL CHECK (document_kind IN ('INVOICE','DEBIT_NOTE')),
    source_document_id  UUID          NOT NULL,
    document_number     VARCHAR(64)   NOT NULL DEFAULT '',
    customer_party_id   UUID          NOT NULL,
    currency            CHAR(3)       NOT NULL,
    original_amount     NUMERIC(20,4) NOT NULL CONSTRAINT ar07_original_positive CHECK (original_amount > 0),
    residual_amount     NUMERIC(20,4) NOT NULL,
    issue_date          DATE          NOT NULL,
    due_date            DATE          NOT NULL,
    business_calendar_id VARCHAR(64)  NOT NULL DEFAULT 'GREGORIAN_UTC',
    state               VARCHAR(20)   NOT NULL DEFAULT 'OPEN'
        CONSTRAINT ar07_state_valid CHECK (state IN ('CREATED','OPEN','PARTIALLY_SETTLED','SETTLED','WRITTEN_OFF','CORRECTED')),
    disputed            BOOLEAN       NOT NULL DEFAULT FALSE,
    collection_hold     BOOLEAN       NOT NULL DEFAULT FALSE,
    hold_reason         VARCHAR(255),
    closed_at           TIMESTAMPTZ,
    version             INTEGER       NOT NULL DEFAULT 1,
    created_by          VARCHAR(255)  NOT NULL,
    created_at          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar07_residual_range CHECK (residual_amount >= 0 AND residual_amount <= original_amount),
    CONSTRAINT ar07_due_after_issue CHECK (due_date >= issue_date),
    CONSTRAINT uq_ar07_source_document UNIQUE (tenant_id, source_document_id)
);
CREATE INDEX IF NOT EXISTS idx_ar07_customer ON ar07_open_items (tenant_id, legal_entity_id, customer_party_id, currency);
CREATE INDEX IF NOT EXISTS idx_ar07_scope ON ar07_open_items (tenant_id, legal_entity_id, open_item_id);

CREATE TABLE IF NOT EXISTS ar07_adjustments (
    adjustment_id   UUID PRIMARY KEY,
    seq             BIGSERIAL     NOT NULL,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    open_item_id    UUID          NOT NULL REFERENCES ar07_open_items(open_item_id),
    kind            VARCHAR(24)   NOT NULL CHECK (kind IN ('CASH_APPLICATION','CASH_UNAPPLICATION','CREDIT_NOTE','REFUND','WRITE_OFF','CORRECTION')),
    amount          NUMERIC(20,4) NOT NULL CONSTRAINT ar07_adj_amount_positive CHECK (amount > 0),
    signed_effect   NUMERIC(20,4) NOT NULL,
    balance_before  NUMERIC(20,4) NOT NULL,
    balance_after   NUMERIC(20,4) NOT NULL CONSTRAINT ar07_adj_balance_nonneg CHECK (balance_after >= 0),
    source_ref_type VARCHAR(64)   NOT NULL CHECK (source_ref_type <> ''),
    source_ref_id   VARCHAR(128)  NOT NULL CHECK (source_ref_id <> ''),
    event_key       VARCHAR(255)  NOT NULL CHECK (event_key <> ''),
    reason          VARCHAR(500)  NOT NULL DEFAULT '',
    reason_code     VARCHAR(64),
    evidence_id     VARCHAR(255),
    actor_id        VARCHAR(255)  NOT NULL,
    version_after   INTEGER       NOT NULL,
    recorded_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar07_adj_chain CHECK (balance_after = balance_before + signed_effect),
    CONSTRAINT ar07_adj_sign CHECK ((kind IN ('CASH_UNAPPLICATION','REFUND') AND signed_effect = amount)
                                 OR (kind IN ('CASH_APPLICATION','CREDIT_NOTE','WRITE_OFF','CORRECTION') AND signed_effect = -amount)),
    CONSTRAINT uq_ar07_adj_event_key UNIQUE (tenant_id, event_key)
);
CREATE INDEX IF NOT EXISTS idx_ar07_adj_item ON ar07_adjustments (tenant_id, open_item_id, seq);

CREATE TABLE IF NOT EXISTS ar07_disputes (
    dispute_id      UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    open_item_id    UUID          NOT NULL REFERENCES ar07_open_items(open_item_id),
    status          VARCHAR(12)   NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    reason_code     VARCHAR(64)   NOT NULL,
    evidence_id     VARCHAR(255)  NOT NULL,
    opened_by       VARCHAR(255)  NOT NULL,
    opened_at       TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    resolution_code VARCHAR(64),
    resolution_evidence_id VARCHAR(255),
    resolved_by     VARCHAR(255),
    resolved_at     TIMESTAMPTZ,
    CONSTRAINT ar07_dispute_resolved_consistent CHECK ((status = 'OPEN' AND resolved_by IS NULL) OR (status = 'RESOLVED' AND resolved_by IS NOT NULL AND resolution_code IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar07_one_open_dispute ON ar07_disputes (tenant_id, open_item_id) WHERE status = 'OPEN';

CREATE TABLE IF NOT EXISTS ar07_item_history (
    history_id      UUID PRIMARY KEY,
    seq             BIGSERIAL     NOT NULL,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    open_item_id    UUID          NOT NULL REFERENCES ar07_open_items(open_item_id),
    event           VARCHAR(32)   NOT NULL,
    from_state      VARCHAR(20),
    to_state        VARCHAR(20),
    residual_after  NUMERIC(20,4) NOT NULL,
    version_after   INTEGER       NOT NULL,
    actor_id        VARCHAR(255)  NOT NULL,
    reason_code     VARCHAR(64),
    evidence_id     VARCHAR(255),
    detail          JSONB,
    at              TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ar07_hist_item ON ar07_item_history (tenant_id, open_item_id, seq);

-- Append-only ledgers.
DROP TRIGGER IF EXISTS ar07_adj_append_only ON ar07_adjustments;
CREATE TRIGGER ar07_adj_append_only BEFORE UPDATE OR DELETE ON ar07_adjustments
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();
DROP TRIGGER IF EXISTS ar07_hist_append_only ON ar07_item_history;
CREATE TRIGGER ar07_hist_append_only BEFORE UPDATE OR DELETE ON ar07_item_history
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- An adjustment must continue the item's ledger chain: balance_before is the previous balance_after (the original
-- amount for the first), so a fabricated or out-of-order ledger row is refused (#19, #20).
CREATE OR REPLACE FUNCTION ar07_guard_adjustment() RETURNS trigger AS $$
DECLARE prev NUMERIC(20,4); orig NUMERIC(20,4);
BEGIN
    SELECT original_amount INTO orig FROM ar07_open_items WHERE open_item_id = NEW.open_item_id AND tenant_id = NEW.tenant_id;
    IF orig IS NULL THEN
        RAISE EXCEPTION 'AR07_ADJUSTMENT_ORPHAN: the adjustment names no open item of this tenant';
    END IF;
    SELECT balance_after INTO prev FROM ar07_adjustments WHERE open_item_id = NEW.open_item_id AND tenant_id = NEW.tenant_id ORDER BY seq DESC LIMIT 1;
    IF prev IS NULL THEN prev := orig; END IF;
    IF NEW.balance_before <> prev THEN
        RAISE EXCEPTION 'AR07_LEDGER_CHAIN_BROKEN: balance_before % does not continue the ledger balance %', NEW.balance_before, prev;
    END IF;
    IF NEW.balance_after > orig THEN
        RAISE EXCEPTION 'AR07_RESIDUAL_ABOVE_ORIGINAL: a residual can never exceed the original amount';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar07_adjustment_guard ON ar07_adjustments;
CREATE TRIGGER ar07_adjustment_guard BEFORE INSERT ON ar07_adjustments
    FOR EACH ROW EXECUTE FUNCTION ar07_guard_adjustment();

-- The open item's identity is frozen, and its residual can move ONLY to the balance_after of a ledger adjustment of the same
-- item recorded for the new version: no free-form balance edit exists at any layer (#20).
CREATE OR REPLACE FUNCTION ar07_guard_open_item() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR07_OPEN_ITEM_NOT_DELETABLE: an open item is never deleted';
    END IF;
    IF NEW.open_item_id <> OLD.open_item_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.document_kind <> OLD.document_kind OR NEW.source_document_id <> OLD.source_document_id
       OR NEW.customer_party_id <> OLD.customer_party_id OR NEW.currency <> OLD.currency
       OR NEW.original_amount <> OLD.original_amount OR NEW.issue_date <> OLD.issue_date OR NEW.due_date <> OLD.due_date
       OR NEW.business_calendar_id <> OLD.business_calendar_id THEN
        RAISE EXCEPTION 'AR07_OPEN_ITEM_IMMUTABLE: the document binding, customer, currency, original amount and dates of an open item never change';
    END IF;
    IF OLD.closed_at IS NOT NULL AND (NEW.residual_amount <> OLD.residual_amount OR NEW.closed_at IS DISTINCT FROM OLD.closed_at) THEN
        RAISE EXCEPTION 'AR07_OPEN_ITEM_CLOSED: a closed open item cannot change';
    END IF;
    IF NEW.residual_amount <> OLD.residual_amount THEN
        IF NOT EXISTS (SELECT 1 FROM ar07_adjustments a WHERE a.tenant_id = NEW.tenant_id AND a.open_item_id = NEW.open_item_id
                       AND a.version_after = NEW.version AND a.balance_after = NEW.residual_amount AND a.balance_before = OLD.residual_amount) THEN
            RAISE EXCEPTION 'AR07_BALANCE_NOT_EDITABLE: a residual changes only through a ledger adjustment that names its owning event';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar07_open_item_guard ON ar07_open_items;
CREATE TRIGGER ar07_open_item_guard BEFORE UPDATE OR DELETE ON ar07_open_items
    FOR EACH ROW EXECUTE FUNCTION ar07_guard_open_item();

-- The residual of a new item equals its original amount (no free-form opening balance).
CREATE OR REPLACE FUNCTION ar07_guard_open_item_insert() RETURNS trigger AS $$
BEGIN
    IF NEW.residual_amount <> NEW.original_amount THEN
        RAISE EXCEPTION 'AR07_OPENING_BALANCE: an open item starts with residual = original amount';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar07_open_item_insert_guard ON ar07_open_items;
CREATE TRIGGER ar07_open_item_insert_guard BEFORE INSERT ON ar07_open_items
    FOR EACH ROW EXECUTE FUNCTION ar07_guard_open_item_insert();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar07_open_items','ar07_adjustments','ar07_disputes','ar07_item_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
