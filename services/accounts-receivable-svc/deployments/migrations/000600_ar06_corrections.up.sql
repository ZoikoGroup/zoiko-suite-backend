-- Migration: 000600_ar06_corrections.up.sql
-- AR-06 Credit / Debit Note (spec ZS-SVC-C-001 section 9). Correction documents that link to an ISSUED source invoice and
-- never edit it (invariants 1 and 11).
--   ar06_numbering_profiles          per (tenant, legal entity, series CN/DN) number format
--   ar06_number_counters             gapless per (tenant, legal entity, series), advanced inside the issue transaction
--   ar06_source_locks                one row per source invoice: the lock that serialises eligibility checks (no joint over-credit)
--   ar06_corrections                 the credit / debit note; lifecycle Draft -> PendingApproval -> Approved -> Issued -> Applied/Settled
--   ar06_correction_lines            lines (writable only before issue)
--   ar06_correction_tax_determinations  tax-determination-svc references + provenance per tax group (writable only before issue)
--   ar06_replacement_links           replacement invoices linked to an issued correction (append-only chain)
--   ar06_correction_history          append-only evidence of every command

CREATE TABLE IF NOT EXISTS ar06_numbering_profiles (
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

CREATE TABLE IF NOT EXISTS ar06_number_counters (
    tenant_id       UUID        NOT NULL,
    legal_entity_id UUID        NOT NULL,
    series          VARCHAR(16) NOT NULL,
    next_number     BIGINT      NOT NULL DEFAULT 1 CHECK (next_number >= 1),
    PRIMARY KEY (tenant_id, legal_entity_id, series)
);

CREATE OR REPLACE FUNCTION ar06_guard_counters() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR06_COUNTER_NOT_DELETABLE: a legal number sequence is never deleted';
    END IF;
    IF NEW.next_number < OLD.next_number THEN
        RAISE EXCEPTION 'AR06_COUNTER_MONOTONIC: a legal number sequence never moves backwards';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar06_counters_guard ON ar06_number_counters;
CREATE TRIGGER ar06_counters_guard BEFORE UPDATE OR DELETE ON ar06_number_counters
    FOR EACH ROW EXECUTE FUNCTION ar06_guard_counters();

-- Row lock per source invoice. Eligibility (source gross minus issued credits) is derived from ar06_corrections under
-- SELECT ... FOR UPDATE of this row, so concurrent credits against one invoice cannot jointly exceed it.
CREATE TABLE IF NOT EXISTS ar06_source_locks (
    tenant_id         UUID NOT NULL,
    source_invoice_id UUID NOT NULL,
    PRIMARY KEY (tenant_id, source_invoice_id)
);

CREATE TABLE IF NOT EXISTS ar06_corrections (
    correction_id             UUID PRIMARY KEY,
    tenant_id                 UUID          NOT NULL,
    legal_entity_id           UUID          NOT NULL,
    customer_party_id         UUID          NOT NULL,
    kind                      VARCHAR(16)   NOT NULL CHECK (kind IN ('CREDIT_NOTE','DEBIT_NOTE')),
    source_invoice_id         UUID          NOT NULL,
    source_invoice_number     VARCHAR(64)   NOT NULL DEFAULT '',
    currency                  VARCHAR(3)    NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    client_reference          VARCHAR(128),

    state                     VARCHAR(16)   NOT NULL DEFAULT 'DRAFT'
        CONSTRAINT ar06_state_valid CHECK (state IN ('DRAFT','PENDING_APPROVAL','APPROVED','ISSUED','APPLIED','SETTLED','CANCELLED')),
    application_state         VARCHAR(16)   NOT NULL DEFAULT 'NOT_APPLIED'
        CONSTRAINT ar06_application_valid CHECK (application_state IN ('NOT_APPLIED','APPLIED')),
    einvoice_state            VARCHAR(16)   NOT NULL DEFAULT 'NOT_REQUIRED'
        CONSTRAINT ar06_einvoice_valid CHECK (einvoice_state IN ('NOT_REQUIRED','REQUIRED','PREPARED','SUBMITTED','PENDING_UNKNOWN','ACCEPTED','REJECTED')),
    accounting_state          VARCHAR(16)   NOT NULL DEFAULT 'NOT_QUEUED'
        CONSTRAINT ar06_accounting_valid CHECK (accounting_state IN ('NOT_QUEUED','QUEUED','POSTED','FAILED','QUARANTINED')),
    accounting_journal_id     VARCHAR(255),
    accounting_error          TEXT,
    accounting_updated_at     TIMESTAMPTZ,

    reason_code               VARCHAR(128)  NOT NULL CHECK (length(btrim(reason_code)) > 0),
    reason_text               TEXT          NOT NULL DEFAULT '',
    evidence_id               VARCHAR(255),
    supply_date               DATE          NOT NULL,
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

    document_number           VARCHAR(64),
    numbering_series          VARCHAR(16),
    sequence_no               BIGINT,
    issue_date                DATE,
    due_date                  DATE,
    issued_by                 VARCHAR(255),
    issued_at                 TIMESTAMPTZ,
    issued_canonical          TEXT,
    issued_content_hash       VARCHAR(64),
    open_item_id              VARCHAR(255),

    version                   INTEGER       NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT ar06_totals_consistent CHECK (gross_total = net_total + tax_total),
    CONSTRAINT ar06_number_only_when_issued CHECK (state IN ('ISSUED','APPLIED','SETTLED')
        OR (document_number IS NULL AND sequence_no IS NULL AND issued_canonical IS NULL AND issued_content_hash IS NULL AND open_item_id IS NULL)),
    CONSTRAINT ar06_issued_facts CHECK (state NOT IN ('ISSUED','APPLIED','SETTLED')
        OR (document_number IS NOT NULL AND numbering_series IS NOT NULL AND sequence_no IS NOT NULL AND issue_date IS NOT NULL
            AND issued_at IS NOT NULL AND issued_by IS NOT NULL AND issued_canonical IS NOT NULL
            AND length(issued_content_hash) = 64 AND open_item_id IS NOT NULL AND net_total > 0)),
    CONSTRAINT ar06_issued_has_accounting CHECK (state NOT IN ('ISSUED','APPLIED','SETTLED') OR accounting_state <> 'NOT_QUEUED'),
    CONSTRAINT ar06_unissued_no_accounting CHECK (state IN ('ISSUED','APPLIED','SETTLED') OR accounting_state = 'NOT_QUEUED'),
    CONSTRAINT ar06_unissued_dimensions CHECK (state IN ('ISSUED','APPLIED','SETTLED')
        OR (application_state = 'NOT_APPLIED' AND einvoice_state = 'NOT_REQUIRED')),
    CONSTRAINT ar06_state_requires_approval CHECK (state NOT IN ('APPROVED','ISSUED','APPLIED','SETTLED')
        OR (approved_content_hash IS NOT NULL AND approved_by IS NOT NULL AND approved_at IS NOT NULL)),
    CONSTRAINT ar06_pending_requires_submitter CHECK (state NOT IN ('PENDING_APPROVAL','APPROVED','ISSUED','APPLIED','SETTLED')
        OR (submitted_by IS NOT NULL AND submitted_at IS NOT NULL)),
    CONSTRAINT ar06_approver_independent CHECK (approved_by IS NULL OR (approved_by <> created_by AND approved_by <> COALESCE(submitted_by, ''))),
    CONSTRAINT ar06_cancel_requires_reason CHECK (state <> 'CANCELLED' OR (cancel_reason IS NOT NULL AND cancelled_by IS NOT NULL AND cancelled_at IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar06_document_number ON ar06_corrections (tenant_id, legal_entity_id, numbering_series, document_number)
    WHERE document_number IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar06_document_sequence ON ar06_corrections (tenant_id, legal_entity_id, numbering_series, sequence_no)
    WHERE sequence_no IS NOT NULL;
-- Duplicate correction suppression: the same client reference against the same source invoice is one live correction.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar06_client_reference ON ar06_corrections (tenant_id, source_invoice_id, kind, client_reference)
    WHERE client_reference IS NOT NULL AND state <> 'CANCELLED';
CREATE INDEX IF NOT EXISTS idx_ar06_source ON ar06_corrections (tenant_id, source_invoice_id, state);

-- Guard: identity and the source linkage never change, nothing is deleted, a cancelled correction is frozen, the lifecycle only
-- makes legal moves, and an issued note's CONTENT never changes (invariant 1): only the state moves forward (Issued -> Applied ->
-- Settled), the application / e-invoice / accounting dimensions, version and updated_at may change.
CREATE OR REPLACE FUNCTION ar06_guard_corrections() RETURNS trigger AS $$
DECLARE
    mutable_keys TEXT[] := ARRAY['application_state','einvoice_state','accounting_state','accounting_journal_id','accounting_error',
        'accounting_updated_at','version','updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AR06_CORRECTION_NOT_DELETABLE: a correction document is never deleted';
    END IF;
    IF NEW.correction_id <> OLD.correction_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
       OR NEW.customer_party_id <> OLD.customer_party_id OR NEW.currency <> OLD.currency OR NEW.kind <> OLD.kind
       OR NEW.source_invoice_id <> OLD.source_invoice_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'AR06_CORRECTION_IDENTITY_IMMUTABLE: tenant, entity, customer, currency, kind, source invoice and creator never change';
    END IF;
    IF OLD.state = 'CANCELLED' THEN
        RAISE EXCEPTION 'AR06_CORRECTION_TERMINAL: a cancelled correction is frozen';
    END IF;
    IF OLD.state IN ('ISSUED','APPLIED','SETTLED') THEN
        IF NEW.state <> OLD.state AND NOT ((OLD.state = 'ISSUED' AND NEW.state = 'APPLIED') OR (OLD.state IN ('ISSUED','APPLIED') AND NEW.state = 'SETTLED')) THEN
            RAISE EXCEPTION 'AR06_ISSUED_CORRECTION_IMMUTABLE: an issued note only moves forward to Applied / Settled';
        END IF;
        IF (to_jsonb(NEW) - 'state' - mutable_keys) IS DISTINCT FROM (to_jsonb(OLD) - 'state' - mutable_keys) THEN
            RAISE EXCEPTION 'AR06_ISSUED_CORRECTION_IMMUTABLE: an issued credit/debit note is never edited in place';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.state <> OLD.state AND NOT (
           (OLD.state = 'DRAFT'            AND NEW.state IN ('PENDING_APPROVAL','CANCELLED'))
        OR (OLD.state = 'PENDING_APPROVAL' AND NEW.state IN ('DRAFT','APPROVED','CANCELLED'))
        OR (OLD.state = 'APPROVED'         AND NEW.state IN ('DRAFT','ISSUED','CANCELLED'))) THEN
        RAISE EXCEPTION 'AR06_ILLEGAL_LIFECYCLE_MOVE: % -> % is not a legal correction lifecycle move', OLD.state, NEW.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar06_corrections_guard ON ar06_corrections;
CREATE TRIGGER ar06_corrections_guard BEFORE UPDATE OR DELETE ON ar06_corrections
    FOR EACH ROW EXECUTE FUNCTION ar06_guard_corrections();

CREATE TABLE IF NOT EXISTS ar06_correction_lines (
    correction_id          UUID          NOT NULL REFERENCES ar06_corrections (correction_id),
    line_no                INTEGER       NOT NULL CHECK (line_no >= 1),
    tenant_id              UUID          NOT NULL,
    source_line_no         INTEGER       CHECK (source_line_no IS NULL OR source_line_no >= 1),
    description            VARCHAR(500)  NOT NULL,
    quantity               NUMERIC(20,4) NOT NULL CHECK (quantity > 0),
    unit_price             NUMERIC(20,4) NOT NULL CHECK (unit_price >= 0),
    net_amount             NUMERIC(20,4) NOT NULL CHECK (net_amount >= 0),
    tax_amount             NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (tax_amount >= 0),
    tax_category           VARCHAR(64)   NOT NULL DEFAULT '',
    product_classification VARCHAR(255)  NOT NULL DEFAULT '',
    supply_kind            VARCHAR(32)   NOT NULL DEFAULT '' CHECK (supply_kind IN ('','GOODS','SERVICES','DIGITAL_SERVICES')),
    PRIMARY KEY (correction_id, line_no)
);

CREATE TABLE IF NOT EXISTS ar06_correction_tax_determinations (
    correction_id          UUID          NOT NULL REFERENCES ar06_corrections (correction_id),
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
    PRIMARY KEY (correction_id, group_no)
);

CREATE OR REPLACE FUNCTION ar06_guard_children() RETURNS trigger AS $$
DECLARE
    st  TEXT;
    cid UUID;
BEGIN
    cid := CASE WHEN TG_OP = 'DELETE' THEN OLD.correction_id ELSE NEW.correction_id END;
    SELECT state INTO st FROM ar06_corrections WHERE correction_id = cid;
    IF st IS NOT NULL AND st NOT IN ('DRAFT','PENDING_APPROVAL','APPROVED') THEN
        RAISE EXCEPTION 'AR06_ISSUED_CORRECTION_IMMUTABLE: the lines and tax result of a % correction are sealed', st;
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ar06_lines_guard ON ar06_correction_lines;
CREATE TRIGGER ar06_lines_guard BEFORE INSERT OR UPDATE OR DELETE ON ar06_correction_lines
    FOR EACH ROW EXECUTE FUNCTION ar06_guard_children();
DROP TRIGGER IF EXISTS ar06_tax_guard ON ar06_correction_tax_determinations;
CREATE TRIGGER ar06_tax_guard BEFORE INSERT OR UPDATE OR DELETE ON ar06_correction_tax_determinations
    FOR EACH ROW EXECUTE FUNCTION ar06_guard_children();

-- The correction chain: replacement invoices linked to an issued correction. Append-only.
CREATE TABLE IF NOT EXISTS ar06_replacement_links (
    link_id                UUID PRIMARY KEY,
    tenant_id              UUID         NOT NULL,
    legal_entity_id        UUID         NOT NULL,
    correction_id          UUID         NOT NULL REFERENCES ar06_corrections (correction_id),
    replacement_invoice_id UUID         NOT NULL,
    replacement_number     VARCHAR(64)  NOT NULL DEFAULT '',
    reason_code            VARCHAR(128),
    linked_by              VARCHAR(255) NOT NULL,
    linked_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_ar06_replacement UNIQUE (tenant_id, correction_id, replacement_invoice_id)
);
DROP TRIGGER IF EXISTS ar06_links_append_only ON ar06_replacement_links;
CREATE TRIGGER ar06_links_append_only BEFORE UPDATE OR DELETE ON ar06_replacement_links
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar06_correction_history (
    history_id          UUID PRIMARY KEY,
    tenant_id           UUID         NOT NULL,
    legal_entity_id     UUID         NOT NULL,
    correction_id       UUID         NOT NULL REFERENCES ar06_corrections (correction_id),
    correction_version  INTEGER      NOT NULL,
    command             VARCHAR(64)  NOT NULL,
    state_after         VARCHAR(16)  NOT NULL,
    actor_id            VARCHAR(255) NOT NULL,
    approver_id         VARCHAR(255),
    reason_code         VARCHAR(128),
    evidence_id         VARCHAR(255),
    content_hash        VARCHAR(64),
    prior_values        JSONB,
    new_values          JSONB,
    correlation_id      VARCHAR(255) NOT NULL DEFAULT '',
    recorded_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (correction_id, correction_version)
);
DROP TRIGGER IF EXISTS ar06_history_append_only ON ar06_correction_history;
CREATE TRIGGER ar06_history_append_only BEFORE UPDATE OR DELETE ON ar06_correction_history
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar06_numbering_profiles','ar06_number_counters','ar06_source_locks','ar06_corrections','ar06_correction_lines',
                             'ar06_correction_tax_determinations','ar06_replacement_links','ar06_correction_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
