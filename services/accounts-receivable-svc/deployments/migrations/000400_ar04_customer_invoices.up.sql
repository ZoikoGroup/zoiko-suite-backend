-- Migration: 000400_ar04_customer_invoices.up.sql
-- AR-04 Customer Invoice (spec ZS-SVC-C-001 section 7). The authoritative receivable / tax document.
-- NEW tables: the legacy customer_invoices table keeps its ISSUED -> SENT -> PAID machine and is not reused.
--   ar04_numbering_profiles          per (tenant, legal entity, series) number format (prefix / width); a controlled,
--                                    jurisdiction-pack decision, never hard-coded globally (a documented default applies when absent)
--   ar04_number_counters             gapless per (tenant, legal entity, series) sequence, advanced inside the issue transaction
--   ar04_invoices                    the invoice; six ORTHOGONAL state dimensions, each its own column with its own CHECK
--   ar04_invoice_lines               lines (writable only before issue)
--   ar04_invoice_tax_determinations  tax-determination-svc references + provenance per tax group (writable only before issue)
--   ar04_invoice_approvals           approvals bound to a content hash (append-only, SoD in the data)
--   ar04_cancellation_requests       legal-cancellation requests (never edits the invoice)
--   ar04_correction_links            correction documents linked to an issued invoice (append-only)
--   ar04_invoice_history             append-only evidence of every command

CREATE TABLE IF NOT EXISTS ar04_numbering_profiles (
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    series          VARCHAR(16)  NOT NULL CHECK (series ~ '^[A-Z0-9_-]{1,16}$'),
    prefix          VARCHAR(16)  NOT NULL DEFAULT '' CHECK (prefix ~ '^[A-Za-z0-9/_.-]{0,16}$'),
    number_width    INTEGER      NOT NULL DEFAULT 6 CHECK (number_width BETWEEN 1 AND 12),
    rule_reference  VARCHAR(255) NOT NULL DEFAULT '',
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, legal_entity_id, series)
);

CREATE TABLE IF NOT EXISTS ar04_number_counters (
    tenant_id       UUID        NOT NULL,
    legal_entity_id UUID        NOT NULL,
    series          VARCHAR(16) NOT NULL,
    next_number     BIGINT      NOT NULL DEFAULT 1 CHECK (next_number >= 1),
    PRIMARY KEY (tenant_id, legal_entity_id, series)
);

-- A legal sequence only moves forward and is never deleted.
CREATE OR REPLACE FUNCTION ar04_guard_counters() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR04_COUNTER_NOT_DELETABLE: a legal number sequence is never deleted';
    END IF;
    IF NEW.next_number < OLD.next_number THEN
        RAISE EXCEPTION 'AR04_COUNTER_MONOTONIC: a legal number sequence never moves backwards';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar04_counters_guard ON ar04_number_counters;
CREATE TRIGGER ar04_counters_guard BEFORE UPDATE OR DELETE ON ar04_number_counters
    FOR EACH ROW EXECUTE FUNCTION ar04_guard_counters();

CREATE TABLE IF NOT EXISTS ar04_invoices (
    invoice_id                UUID PRIMARY KEY,
    tenant_id                 UUID          NOT NULL,
    legal_entity_id           UUID          NOT NULL,
    customer_party_id         UUID          NOT NULL,
    currency                  VARCHAR(3)    NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    -- six orthogonal dimensions (invariant 4): one column and one CHECK each, one writer each
    lifecycle_state           VARCHAR(16)   NOT NULL DEFAULT 'DRAFT'
        CONSTRAINT ar04_lifecycle_valid CHECK (lifecycle_state IN ('DRAFT','PENDING_APPROVAL','APPROVED','ISSUED','CORRECTED','CANCELLED')),
    delivery_state            VARCHAR(16)   NOT NULL DEFAULT 'NOT_SENT'
        CONSTRAINT ar04_delivery_valid CHECK (delivery_state IN ('NOT_SENT','SENT','DELIVERY_FAILED','DELIVERED')),
    einvoice_state            VARCHAR(16)   NOT NULL DEFAULT 'NOT_REQUIRED'
        CONSTRAINT ar04_einvoice_valid CHECK (einvoice_state IN ('NOT_REQUIRED','REQUIRED','PREPARED','SUBMITTED','PENDING_UNKNOWN','ACCEPTED','REJECTED')),
    settlement_state          VARCHAR(16)   NOT NULL DEFAULT 'UNPAID'
        CONSTRAINT ar04_settlement_valid CHECK (settlement_state IN ('UNPAID','PARTIALLY_PAID','PAID','WRITTEN_OFF','CREDITED')),
    dispute_state             VARCHAR(16)   NOT NULL DEFAULT 'NONE'
        CONSTRAINT ar04_dispute_valid CHECK (dispute_state IN ('NONE','DISPUTED')),
    attention_state           VARCHAR(32)   NOT NULL DEFAULT 'NONE'
        CONSTRAINT ar04_attention_valid CHECK (attention_state IN ('NONE','CANCELLATION_REQUESTED','CANCELLATION_APPROVED','CORRECTION_LINKED')),

    -- accounting mirror of the ACC-04 posting outcome (written by the posting listener only)
    accounting_state          VARCHAR(16)   NOT NULL DEFAULT 'NOT_QUEUED'
        CONSTRAINT ar04_accounting_valid CHECK (accounting_state IN ('NOT_QUEUED','QUEUED','POSTED','FAILED','QUARANTINED')),
    accounting_journal_id     VARCHAR(255),
    accounting_error          TEXT,
    accounting_updated_at     TIMESTAMPTZ,

    invoice_number            VARCHAR(64),
    numbering_series          VARCHAR(16),
    sequence_no               BIGINT,
    supply_date               DATE          NOT NULL,
    requested_due_date        DATE,
    issue_date                DATE,
    due_date                  DATE,
    payment_terms_code        VARCHAR(64),
    payment_terms_days        INTEGER,
    customer_reference        VARCHAR(255)  NOT NULL DEFAULT '',
    notes                     TEXT          NOT NULL DEFAULT '',
    order_id                  UUID,
    milestone_id              UUID,
    tax_jurisdiction_id       VARCHAR(255)  NOT NULL DEFAULT '',
    tax_supply_jurisdiction_id VARCHAR(255) NOT NULL DEFAULT '',
    tax_supply_type           VARCHAR(8)    NOT NULL DEFAULT '',
    tax_buyer_registration_id VARCHAR(255)  NOT NULL DEFAULT '',

    net_total                 NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (net_total >= 0),
    tax_total                 NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (tax_total >= 0),
    gross_total               NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (gross_total >= 0),

    content_hash              VARCHAR(64)   NOT NULL,
    approved_content_hash     VARCHAR(64),
    approved_by               VARCHAR(255),
    approved_at               TIMESTAMPTZ,
    created_by                VARCHAR(255)  NOT NULL,
    submitted_by              VARCHAR(255),
    submitted_at              TIMESTAMPTZ,
    cancel_reason             VARCHAR(128),
    cancelled_by              VARCHAR(255),
    cancelled_at              TIMESTAMPTZ,

    -- the sealed issued representation: exact canonical JSON text and its sha256
    issued_by                 VARCHAR(255),
    issued_at                 TIMESTAMPTZ,
    issued_canonical          TEXT,
    issued_content_hash       VARCHAR(64),
    open_item_id              VARCHAR(255),

    delivery_reference        VARCHAR(255),
    delivery_detail           TEXT,
    delivery_changed_at       TIMESTAMPTZ,
    settlement_changed_at     TIMESTAMPTZ,

    version                   INTEGER       NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT ar04_totals_consistent CHECK (gross_total = net_total + tax_total),
    CONSTRAINT ar04_order_link_pair CHECK ((order_id IS NULL) = (milestone_id IS NULL)),
    -- the other dimensions only leave their initial values once the invoice is issued
    CONSTRAINT ar04_dimensions_only_when_issued CHECK (lifecycle_state IN ('ISSUED','CORRECTED')
        OR (delivery_state = 'NOT_SENT' AND einvoice_state = 'NOT_REQUIRED' AND settlement_state = 'UNPAID'
            AND dispute_state = 'NONE' AND attention_state = 'NONE')),
    CONSTRAINT ar04_number_only_when_issued CHECK (lifecycle_state IN ('ISSUED','CORRECTED')
        OR (invoice_number IS NULL AND sequence_no IS NULL AND issued_canonical IS NULL AND issued_content_hash IS NULL AND open_item_id IS NULL)),
    CONSTRAINT ar04_issued_facts CHECK (lifecycle_state NOT IN ('ISSUED','CORRECTED')
        OR (invoice_number IS NOT NULL AND numbering_series IS NOT NULL AND sequence_no IS NOT NULL AND issue_date IS NOT NULL
            AND due_date IS NOT NULL AND issued_at IS NOT NULL AND issued_by IS NOT NULL AND issued_canonical IS NOT NULL
            AND length(issued_content_hash) = 64 AND open_item_id IS NOT NULL AND net_total > 0)),
    -- an issued invoice always has an accounting obligation queued in the same transaction
    CONSTRAINT ar04_issued_has_accounting CHECK (lifecycle_state NOT IN ('ISSUED','CORRECTED') OR accounting_state <> 'NOT_QUEUED'),
    CONSTRAINT ar04_unissued_no_accounting CHECK (lifecycle_state IN ('ISSUED','CORRECTED') OR accounting_state = 'NOT_QUEUED'),
    CONSTRAINT ar04_state_requires_approval CHECK (lifecycle_state NOT IN ('APPROVED','ISSUED','CORRECTED')
        OR (approved_content_hash IS NOT NULL AND approved_by IS NOT NULL AND approved_at IS NOT NULL)),
    CONSTRAINT ar04_pending_requires_submitter CHECK (lifecycle_state NOT IN ('PENDING_APPROVAL','APPROVED','ISSUED','CORRECTED')
        OR (submitted_by IS NOT NULL AND submitted_at IS NOT NULL)),
    -- maker-checker: the approver is neither the creator nor the submitter
    CONSTRAINT ar04_approver_independent CHECK (approved_by IS NULL OR (approved_by <> created_by AND approved_by <> COALESCE(submitted_by, ''))),
    CONSTRAINT ar04_cancel_requires_reason CHECK (lifecycle_state <> 'CANCELLED' OR (cancel_reason IS NOT NULL AND cancelled_by IS NOT NULL AND cancelled_at IS NOT NULL))
);
-- Zero duplicate numbers: unique per (tenant, legal entity, series), both as the formatted number and the sequence value.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar04_invoice_number ON ar04_invoices (tenant_id, legal_entity_id, numbering_series, invoice_number)
    WHERE invoice_number IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar04_invoice_sequence ON ar04_invoices (tenant_id, legal_entity_id, numbering_series, sequence_no)
    WHERE sequence_no IS NOT NULL;
-- A billable milestone backs at most one issued invoice (backstop to AR-03's invoiced ledger).
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar04_issued_milestone ON ar04_invoices (tenant_id, milestone_id)
    WHERE milestone_id IS NOT NULL AND lifecycle_state IN ('ISSUED','CORRECTED');
CREATE INDEX IF NOT EXISTS idx_ar04_invoices_customer ON ar04_invoices (tenant_id, legal_entity_id, customer_party_id);
CREATE INDEX IF NOT EXISTS idx_ar04_invoices_entity_state ON ar04_invoices (tenant_id, legal_entity_id, lifecycle_state);

-- Guard: identity never changes, nothing is deleted, a cancelled invoice is frozen, the lifecycle only
-- makes legal moves, and an ISSUED / CORRECTED invoice's CONTENT can never change (invariant 1): only the
-- orthogonal dimension columns, the accounting mirror, the delivery / settlement detail and the
-- version / updated_at bookkeeping may change.
CREATE OR REPLACE FUNCTION ar04_guard_invoices() RETURNS trigger AS $$
DECLARE
    mutable_keys TEXT[] := ARRAY['delivery_state','einvoice_state','settlement_state','dispute_state','attention_state',
        'accounting_state','accounting_journal_id','accounting_error','accounting_updated_at',
        'delivery_reference','delivery_detail','delivery_changed_at','settlement_changed_at','version','updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR04_INVOICE_NOT_DELETABLE: an invoice is never deleted';
    END IF;
    IF NEW.invoice_id <> OLD.invoice_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.customer_party_id <> OLD.customer_party_id OR NEW.currency <> OLD.currency
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'AR04_INVOICE_IDENTITY_IMMUTABLE: tenant, entity, customer, currency and creator never change';
    END IF;
    IF OLD.lifecycle_state = 'CANCELLED' THEN
        RAISE EXCEPTION 'AR04_INVOICE_TERMINAL: a cancelled invoice is frozen';
    END IF;
    IF OLD.lifecycle_state IN ('ISSUED','CORRECTED') THEN
        IF NEW.lifecycle_state <> OLD.lifecycle_state AND NOT (OLD.lifecycle_state = 'ISSUED' AND NEW.lifecycle_state = 'CORRECTED') THEN
            RAISE EXCEPTION 'AR04_ISSUED_INVOICE_IMMUTABLE: an issued invoice only moves to CORRECTED (via a correction document)';
        END IF;
        IF (to_jsonb(NEW) - 'lifecycle_state' - mutable_keys) IS DISTINCT FROM (to_jsonb(OLD) - 'lifecycle_state' - mutable_keys) THEN
            RAISE EXCEPTION 'AR04_ISSUED_INVOICE_IMMUTABLE: an issued invoice is never edited in place; use a correction document';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.lifecycle_state <> OLD.lifecycle_state AND NOT (
           (OLD.lifecycle_state = 'DRAFT'            AND NEW.lifecycle_state IN ('PENDING_APPROVAL','CANCELLED'))
        OR (OLD.lifecycle_state = 'PENDING_APPROVAL' AND NEW.lifecycle_state IN ('DRAFT','APPROVED','CANCELLED'))
        OR (OLD.lifecycle_state = 'APPROVED'         AND NEW.lifecycle_state IN ('DRAFT','ISSUED','CANCELLED'))) THEN
        RAISE EXCEPTION 'AR04_ILLEGAL_LIFECYCLE_MOVE: % -> % is not a legal invoice lifecycle move', OLD.lifecycle_state, NEW.lifecycle_state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar04_invoices_guard ON ar04_invoices;
CREATE TRIGGER ar04_invoices_guard BEFORE UPDATE OR DELETE ON ar04_invoices
    FOR EACH ROW EXECUTE FUNCTION ar04_guard_invoices();

CREATE TABLE IF NOT EXISTS ar04_invoice_lines (
    invoice_id             UUID          NOT NULL REFERENCES ar04_invoices (invoice_id),
    line_no                INTEGER       NOT NULL CHECK (line_no >= 1),
    tenant_id              UUID          NOT NULL,
    description            VARCHAR(500)  NOT NULL,
    product_ref            VARCHAR(255)  NOT NULL DEFAULT '',
    quantity               NUMERIC(20,4) NOT NULL CHECK (quantity > 0),
    unit_price             NUMERIC(20,4) NOT NULL CHECK (unit_price >= 0),
    net_amount             NUMERIC(20,4) NOT NULL CHECK (net_amount >= 0),
    tax_amount             NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (tax_amount >= 0),
    tax_category           VARCHAR(64)   NOT NULL DEFAULT '',
    product_classification VARCHAR(255)  NOT NULL DEFAULT '',
    supply_kind            VARCHAR(32)   NOT NULL DEFAULT '' CHECK (supply_kind IN ('','GOODS','SERVICES','DIGITAL_SERVICES')),
    PRIMARY KEY (invoice_id, line_no)
);

CREATE TABLE IF NOT EXISTS ar04_invoice_tax_determinations (
    invoice_id             UUID          NOT NULL REFERENCES ar04_invoices (invoice_id),
    group_no               INTEGER       NOT NULL CHECK (group_no >= 1),
    tenant_id              UUID          NOT NULL,
    tax_category           VARCHAR(64)   NOT NULL,
    product_classification VARCHAR(255)  NOT NULL,
    supply_kind            VARCHAR(32)   NOT NULL,
    determination_id       VARCHAR(255)  NOT NULL CHECK (length(btrim(determination_id)) > 0),
    provenance             VARCHAR(255)  NOT NULL CHECK (length(btrim(provenance)) > 0),
    determination_status   VARCHAR(32)   NOT NULL,
    jurisdiction_id        VARCHAR(255)  NOT NULL DEFAULT '',
    tax_rate               VARCHAR(32)   NOT NULL DEFAULT '',
    net_amount             NUMERIC(20,4) NOT NULL CHECK (net_amount >= 0),
    taxable_amount         NUMERIC(20,4) NOT NULL CHECK (taxable_amount >= 0),
    tax_amount             NUMERIC(20,4) NOT NULL CHECK (tax_amount >= 0),
    recorded_by            VARCHAR(255)  NOT NULL,
    recorded_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (invoice_id, group_no)
);

-- Lines and tax references change only while the invoice is pre-issue; an issued, corrected or cancelled
-- invoice's lines and tax result are sealed.
CREATE OR REPLACE FUNCTION ar04_guard_children() RETURNS trigger AS $$
DECLARE
    st  TEXT;
    inv UUID;
BEGIN
    inv := CASE WHEN TG_OP = 'DELETE' THEN OLD.invoice_id ELSE NEW.invoice_id END;
    SELECT lifecycle_state INTO st FROM ar04_invoices WHERE invoice_id = inv;
    IF st IS NOT NULL AND st NOT IN ('DRAFT','PENDING_APPROVAL','APPROVED') THEN
        RAISE EXCEPTION 'AR04_ISSUED_INVOICE_IMMUTABLE: the lines and tax result of a % invoice are sealed', st;
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar04_lines_guard ON ar04_invoice_lines;
CREATE TRIGGER ar04_lines_guard BEFORE INSERT OR UPDATE OR DELETE ON ar04_invoice_lines
    FOR EACH ROW EXECUTE FUNCTION ar04_guard_children();
DROP TRIGGER IF EXISTS ar04_tax_guard ON ar04_invoice_tax_determinations;
CREATE TRIGGER ar04_tax_guard BEFORE INSERT OR UPDATE OR DELETE ON ar04_invoice_tax_determinations
    FOR EACH ROW EXECUTE FUNCTION ar04_guard_children();

CREATE TABLE IF NOT EXISTS ar04_invoice_approvals (
    approval_id     UUID PRIMARY KEY,
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    invoice_id      UUID          NOT NULL REFERENCES ar04_invoices (invoice_id),
    content_hash    VARCHAR(64)   NOT NULL,
    gross_total     NUMERIC(20,4) NOT NULL,
    created_by      VARCHAR(255)  NOT NULL,
    submitted_by    VARCHAR(255)  NOT NULL,
    approved_by     VARCHAR(255)  NOT NULL,
    approved_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar04_approval_sod CHECK (approved_by <> created_by AND approved_by <> submitted_by)
);
CREATE INDEX IF NOT EXISTS idx_ar04_approvals_invoice ON ar04_invoice_approvals (invoice_id, approved_at);
DROP TRIGGER IF EXISTS ar04_approvals_append_only ON ar04_invoice_approvals;
CREATE TRIGGER ar04_approvals_append_only BEFORE UPDATE OR DELETE ON ar04_invoice_approvals
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- A legal-cancellation request NEVER edits or voids the invoice (the jurisdiction semantics are a
-- correction document, AR-06): it is a request with reason and evidence and a decision by a different principal.
CREATE TABLE IF NOT EXISTS ar04_cancellation_requests (
    request_id      UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    invoice_id      UUID         NOT NULL REFERENCES ar04_invoices (invoice_id),
    status          VARCHAR(16)  NOT NULL CHECK (status IN ('REQUESTED','APPROVED','REJECTED','FULFILLED')),
    reason_code     VARCHAR(128) NOT NULL CHECK (length(btrim(reason_code)) > 0),
    evidence_id     VARCHAR(255) NOT NULL CHECK (length(btrim(evidence_id)) > 0),
    requested_by    VARCHAR(255) NOT NULL,
    requested_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    decided_by      VARCHAR(255),
    decided_at      TIMESTAMPTZ,
    decision_reason VARCHAR(128),
    CONSTRAINT ar04_cancel_decision_facts CHECK (status = 'REQUESTED' OR (decided_by IS NOT NULL AND decided_at IS NOT NULL)),
    CONSTRAINT ar04_cancel_sod CHECK (decided_by IS NULL OR decided_by <> requested_by)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar04_open_cancellation ON ar04_cancellation_requests (tenant_id, invoice_id)
    WHERE status IN ('REQUESTED','APPROVED');
CREATE OR REPLACE FUNCTION ar04_guard_cancellations() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR04_CANCELLATION_NOT_DELETABLE: a cancellation request is never deleted';
    END IF;
    IF OLD.status IN ('REJECTED','FULFILLED') THEN
        RAISE EXCEPTION 'AR04_CANCELLATION_FINAL: a % cancellation request cannot change', OLD.status;
    END IF;
    IF NEW.request_id <> OLD.request_id OR NEW.invoice_id <> OLD.invoice_id OR NEW.reason_code <> OLD.reason_code
       OR NEW.evidence_id <> OLD.evidence_id OR NEW.requested_by <> OLD.requested_by OR NEW.requested_at <> OLD.requested_at THEN
        RAISE EXCEPTION 'AR04_CANCELLATION_IMMUTABLE: the request itself never changes, only its decision';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar04_cancellations_guard ON ar04_cancellation_requests;
CREATE TRIGGER ar04_cancellations_guard BEFORE UPDATE OR DELETE ON ar04_cancellation_requests
    FOR EACH ROW EXECUTE FUNCTION ar04_guard_cancellations();

CREATE TABLE IF NOT EXISTS ar04_correction_links (
    link_id         UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    invoice_id      UUID         NOT NULL REFERENCES ar04_invoices (invoice_id),
    correction_id   VARCHAR(255) NOT NULL CHECK (length(btrim(correction_id)) > 0),
    kind            VARCHAR(16)  NOT NULL CHECK (kind IN ('CREDIT_NOTE','DEBIT_NOTE','FULL_CORRECTION')),
    reason_code     VARCHAR(128),
    evidence_id     VARCHAR(255),
    linked_by       VARCHAR(255) NOT NULL,
    linked_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar04_correction_link UNIQUE (tenant_id, invoice_id, correction_id)
);
DROP TRIGGER IF EXISTS ar04_links_append_only ON ar04_correction_links;
CREATE TRIGGER ar04_links_append_only BEFORE UPDATE OR DELETE ON ar04_correction_links
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar04_invoice_history (
    history_id       UUID PRIMARY KEY,
    tenant_id        UUID         NOT NULL,
    legal_entity_id  UUID         NOT NULL,
    invoice_id       UUID         NOT NULL REFERENCES ar04_invoices (invoice_id),
    invoice_version  INTEGER      NOT NULL,
    command          VARCHAR(64)  NOT NULL,
    lifecycle_after  VARCHAR(16)  NOT NULL,
    actor_id         VARCHAR(255) NOT NULL,
    approver_id      VARCHAR(255),
    reason_code      VARCHAR(128),
    evidence_id      VARCHAR(255),
    content_hash     VARCHAR(64),
    prior_values     JSONB,
    new_values       JSONB,
    correlation_id   VARCHAR(255) NOT NULL DEFAULT '',
    recorded_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (invoice_id, invoice_version)
);
DROP TRIGGER IF EXISTS ar04_history_append_only ON ar04_invoice_history;
CREATE TRIGGER ar04_history_append_only BEFORE UPDATE OR DELETE ON ar04_invoice_history
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar04_numbering_profiles','ar04_number_counters','ar04_invoices','ar04_invoice_lines','ar04_invoice_tax_determinations',
                             'ar04_invoice_approvals','ar04_cancellation_requests','ar04_correction_links','ar04_invoice_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
