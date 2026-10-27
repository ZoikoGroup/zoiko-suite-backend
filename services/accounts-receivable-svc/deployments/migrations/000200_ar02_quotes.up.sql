-- Migration: 000200_ar02_quotes.up.sql
-- AR-02 Quote / Estimate (spec ZS-SVC-C-001 section 5). NON-POSTING: nothing here
-- queues an accounting posting or creates a receivable.
--   ar02_quote_counters        per (tenant, legal entity) quote number sequence
--   ar02_quotes                stable quote header + lifecycle facts
--   ar02_quote_versions        immutable content versions (superseded versions preserved)
--   ar02_quote_lines           immutable lines of a version
--   ar02_quote_tax_estimates   optional TAX estimate provenance per version (append-only)
--   ar02_quote_approvals       approvals bound to a version content hash (append-only)
--   ar02_quote_history         append-only evidence of every command

CREATE TABLE IF NOT EXISTS ar02_quote_counters (
    tenant_id       UUID    NOT NULL,
    legal_entity_id UUID    NOT NULL,
    last_no         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, legal_entity_id)
);

CREATE TABLE IF NOT EXISTS ar02_quotes (
    quote_id               UUID PRIMARY KEY,
    tenant_id              UUID          NOT NULL,
    legal_entity_id        UUID          NOT NULL,
    quote_number           VARCHAR(32)   NOT NULL,
    customer_party_id      UUID          NOT NULL,
    currency               VARCHAR(3)    NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    state                  VARCHAR(16)   NOT NULL CHECK (state IN ('DRAFT','PENDING_APPROVAL','APPROVED','SENT','ACCEPTED','REJECTED','EXPIRED','CONVERTED')),
    current_version_no     INTEGER       NOT NULL CHECK (current_version_no >= 1),
    version                INTEGER       NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_by             VARCHAR(255)  NOT NULL,
    submitted_by           VARCHAR(255),
    approved_version_no    INTEGER,
    approved_content_hash  VARCHAR(64),
    approved_by            VARCHAR(255),
    approved_at            TIMESTAMPTZ,
    sent_at                TIMESTAMPTZ,
    sent_version_no        INTEGER,
    accepted_at            TIMESTAMPTZ,
    accepted_by            VARCHAR(255),
    acceptance_channel     VARCHAR(32),
    acceptance_evidence_id VARCHAR(255),
    accepted_version_no    INTEGER,
    rejected_at            TIMESTAMPTZ,
    rejection_reason       VARCHAR(128),
    expired_at             TIMESTAMPTZ,
    converted_order_id     VARCHAR(255),
    converted_at           TIMESTAMPTZ,
    converted_version_no   INTEGER,
    created_at             TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, legal_entity_id, quote_number),
    -- An approval-dependent state always carries the approved content hash (an approval
    -- invalidated by a material change clears it and the quote returns to DRAFT).
    CONSTRAINT ar02_state_requires_approval CHECK (state NOT IN ('APPROVED','SENT','ACCEPTED','CONVERTED') OR approved_content_hash IS NOT NULL),
    -- Acceptance cannot be fabricated: it carries who accepted, how, and evidence.
    CONSTRAINT ar02_acceptance_requires_evidence CHECK (state NOT IN ('ACCEPTED','CONVERTED')
        OR (acceptance_evidence_id IS NOT NULL AND accepted_by IS NOT NULL AND acceptance_channel IS NOT NULL)),
    CONSTRAINT ar02_converted_requires_order CHECK (state <> 'CONVERTED' OR converted_order_id IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar02_quote_converted_order
    ON ar02_quotes (tenant_id, converted_order_id) WHERE converted_order_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ar02_quotes_list ON ar02_quotes (tenant_id, created_at DESC, quote_id);
CREATE INDEX IF NOT EXISTS idx_ar02_quotes_customer ON ar02_quotes (tenant_id, legal_entity_id, customer_party_id);

CREATE TABLE IF NOT EXISTS ar02_quote_versions (
    quote_id             UUID          NOT NULL REFERENCES ar02_quotes (quote_id),
    version_no           INTEGER       NOT NULL CHECK (version_no >= 1),
    tenant_id            UUID          NOT NULL,
    legal_entity_id      UUID          NOT NULL,
    currency             VARCHAR(3)    NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    validity_date        DATE          NOT NULL,
    terms                TEXT          NOT NULL DEFAULT '',
    assumptions          TEXT          NOT NULL DEFAULT '',
    discount_percent     NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (discount_percent >= 0 AND discount_percent <= 100),
    price_source_ref     VARCHAR(255)  NOT NULL DEFAULT '',
    catalog_version_ref  VARCHAR(255)  NOT NULL DEFAULT '',
    request_tax_estimate BOOLEAN       NOT NULL DEFAULT FALSE,
    tax_params           JSONB,
    subtotal             NUMERIC(20,4) NOT NULL CHECK (subtotal >= 0),
    discount_amount      NUMERIC(20,4) NOT NULL CHECK (discount_amount >= 0),
    net_total            NUMERIC(20,4) NOT NULL CHECK (net_total >= 0),
    content_hash         VARCHAR(64)   NOT NULL,
    change_kind          VARCHAR(16)   NOT NULL CHECK (change_kind IN ('CREATE','AMEND')),
    material_change      BOOLEAN       NOT NULL DEFAULT FALSE,
    created_by           VARCHAR(255)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (quote_id, version_no),
    CONSTRAINT ar02_totals_consistent CHECK (net_total = subtotal - discount_amount)
);

-- Versions are recorded in sequence; once written they are never changed or removed.
CREATE OR REPLACE FUNCTION ar02_check_version_seq() RETURNS trigger AS $$
DECLARE
    mx INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.quote_id::text, 200));
    SELECT max(version_no) INTO mx FROM ar02_quote_versions WHERE quote_id = NEW.quote_id;
    IF (mx IS NULL AND NEW.version_no <> 1) OR (mx IS NOT NULL AND NEW.version_no <> mx + 1) THEN
        RAISE EXCEPTION 'AR02_VERSION_SEQUENCE: version_no % is not the next version', NEW.version_no;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ar02_versions_seq ON ar02_quote_versions;
CREATE TRIGGER ar02_versions_seq BEFORE INSERT ON ar02_quote_versions
    FOR EACH ROW EXECUTE FUNCTION ar02_check_version_seq();
DROP TRIGGER IF EXISTS ar02_versions_immutable ON ar02_quote_versions;
CREATE TRIGGER ar02_versions_immutable BEFORE UPDATE OR DELETE ON ar02_quote_versions
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar02_quote_lines (
    quote_id        UUID          NOT NULL,
    version_no      INTEGER       NOT NULL,
    line_no         INTEGER       NOT NULL CHECK (line_no >= 1),
    tenant_id       UUID          NOT NULL,
    description     VARCHAR(500)  NOT NULL,
    product_ref     VARCHAR(255)  NOT NULL DEFAULT '',
    quantity        NUMERIC(20,4) NOT NULL CHECK (quantity > 0),
    unit_price      NUMERIC(20,4) NOT NULL CHECK (unit_price >= 0),
    line_net        NUMERIC(20,4) NOT NULL CHECK (line_net >= 0),
    PRIMARY KEY (quote_id, version_no, line_no),
    FOREIGN KEY (quote_id, version_no) REFERENCES ar02_quote_versions (quote_id, version_no)
);
DROP TRIGGER IF EXISTS ar02_lines_immutable ON ar02_quote_lines;
CREATE TRIGGER ar02_lines_immutable BEFORE UPDATE OR DELETE ON ar02_quote_lines
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- Optional TAX estimate for one version: an estimate only, never a tax document.
CREATE TABLE IF NOT EXISTS ar02_quote_tax_estimates (
    quote_id          UUID          NOT NULL,
    version_no        INTEGER       NOT NULL,
    tenant_id         UUID          NOT NULL,
    legal_entity_id   UUID          NOT NULL,
    determination_id  VARCHAR(255)  NOT NULL,
    provenance        VARCHAR(255)  NOT NULL DEFAULT '',
    status            VARCHAR(32)   NOT NULL,
    taxable_amount    NUMERIC(20,4) NOT NULL,
    tax_rate          VARCHAR(32)   NOT NULL DEFAULT '',
    tax_amount        NUMERIC(20,4) NOT NULL,
    currency          VARCHAR(3)    NOT NULL,
    jurisdiction_id   VARCHAR(255)  NOT NULL DEFAULT '',
    is_estimate       BOOLEAN       NOT NULL DEFAULT TRUE CHECK (is_estimate),
    recorded_by       VARCHAR(255)  NOT NULL,
    recorded_at       TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (quote_id, version_no),
    FOREIGN KEY (quote_id, version_no) REFERENCES ar02_quote_versions (quote_id, version_no)
);
DROP TRIGGER IF EXISTS ar02_tax_estimates_append_only ON ar02_quote_tax_estimates;
CREATE TRIGGER ar02_tax_estimates_append_only BEFORE UPDATE OR DELETE ON ar02_quote_tax_estimates
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- An approval is bound to the exact content hash of one version. Segregation of
-- duties is enforced in the data too: a discount beyond the preparer's own
-- authority is never approved by the principal who prepared the quote.
CREATE TABLE IF NOT EXISTS ar02_quote_approvals (
    approval_id                 UUID PRIMARY KEY,
    tenant_id                   UUID          NOT NULL,
    legal_entity_id             UUID          NOT NULL,
    quote_id                    UUID          NOT NULL,
    version_no                  INTEGER       NOT NULL,
    content_hash                VARCHAR(64)   NOT NULL,
    discount_percent            NUMERIC(20,4) NOT NULL,
    requires_discount_authority BOOLEAN       NOT NULL,
    prepared_by                 VARCHAR(255)  NOT NULL,
    approved_by                 VARCHAR(255)  NOT NULL,
    approved_at                 TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    FOREIGN KEY (quote_id, version_no) REFERENCES ar02_quote_versions (quote_id, version_no),
    CONSTRAINT ar02_approval_sod CHECK (NOT requires_discount_authority OR approved_by <> prepared_by)
);
CREATE INDEX IF NOT EXISTS idx_ar02_approvals_quote ON ar02_quote_approvals (quote_id, version_no);
DROP TRIGGER IF EXISTS ar02_approvals_append_only ON ar02_quote_approvals;
CREATE TRIGGER ar02_approvals_append_only BEFORE UPDATE OR DELETE ON ar02_quote_approvals
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

CREATE TABLE IF NOT EXISTS ar02_quote_history (
    history_id      UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    quote_id        UUID         NOT NULL REFERENCES ar02_quotes (quote_id),
    quote_version   INTEGER      NOT NULL,
    version_no      INTEGER      NOT NULL,
    command         VARCHAR(64)  NOT NULL,
    state_after     VARCHAR(16)  NOT NULL,
    actor_id        VARCHAR(255) NOT NULL,
    approver_id     VARCHAR(255),
    reason_code     VARCHAR(128),
    evidence_id     VARCHAR(255),
    content_hash    VARCHAR(64),
    prior_values    JSONB,
    new_values      JSONB,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (quote_id, quote_version)
);
DROP TRIGGER IF EXISTS ar02_history_append_only ON ar02_quote_history;
CREATE TRIGGER ar02_history_append_only BEFORE UPDATE OR DELETE ON ar02_quote_history
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar02_quote_counters','ar02_quotes','ar02_quote_versions','ar02_quote_lines','ar02_quote_tax_estimates',
                             'ar02_quote_approvals','ar02_quote_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
