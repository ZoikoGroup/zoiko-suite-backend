-- 0013_vendor_due_diligence_svc.sql
-- vendor-due-diligence-svc → schema `vendor_due_diligence`
--
-- Squashed end state of 000001_initial_schema and 000002_screening_source.
-- Two tables: vendor_dd_checks, vendor_dd_evidence.
--
-- The two backfill UPDATEs in 000002 are dropped rather than translated: one
-- stamped screening_source on already-COMPLETED rows, the other normalised
-- empty-string document_reference to NULL. Both correct data written before
-- those rules existed, and this database has none.

CREATE SCHEMA IF NOT EXISTS vendor_due_diligence;

COMMENT ON SCHEMA vendor_due_diligence IS
    'vendor-due-diligence-svc. Counterparty screening checks and the evidence recorded against them.';

GRANT USAGE ON SCHEMA vendor_due_diligence TO zoiko_backend, authenticated;

-- ── vendor_dd_checks ─────────────────────────────────────────────────────────

CREATE TABLE vendor_due_diligence.vendor_dd_checks (
    check_id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id           VARCHAR(255) NOT NULL,

    counterparty_id           VARCHAR(255) NOT NULL,
    vendor_name               VARCHAR(255) NOT NULL,

    -- STARTED | COMPLETED | FAILED
    status                    VARCHAR(20)  NOT NULL,

    -- CLEAR | FLAGGED — NULL until the check concludes.
    risk_outcome              VARCHAR(20),

    screening_basis           TEXT,

    -- WHICH screening produced the outcome. Without it, CLEAR is
    -- indistinguishable from a real sanctions clearance — and it is not one:
    -- the only screening this service performs is an exact, case-insensitive
    -- match against a hardcoded list of two names. There is no sanctions feed
    -- on the platform to call (external-data-feed-svc carries MARKET_DATA,
    -- CREDIT_SCORE, COMPANY_INFO, FX_RATE and ESG_DATA only).
    --
    -- A consumer cannot avoid over-reading CLEAR unless the record says what
    -- ran, and parsing that out of screening_basis prose is not a contract.
    -- When a real feed is integrated it becomes a second value here and every
    -- historical row stays honest about having been screened by the stub.
    screening_source          VARCHAR(50),

    correlation_id            VARCHAR(255) NOT NULL,
    initiated_by_principal_id VARCHAR(255) NOT NULL DEFAULT app.current_principal_id(),
    started_at                TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at              TIMESTAMPTZ,

    -- The state machine the handler applies, enforced. Without it a partially
    -- applied conclusion is representable — and one was: a completion that
    -- failed midway left STARTED rows the register could not distinguish from
    -- anything else.
    --
    -- `risk_outcome IS NOT NULL` is NOT redundant beside the IN list. A CHECK
    -- rejects a row only when its expression is FALSE, and
    -- `NULL IN ('CLEAR','FLAGGED')` is NULL — so with the IN test alone the
    -- COMPLETED branch went NULL, the disjunction went FALSE OR FALSE OR NULL
    -- = NULL, and a COMPLETED check carrying no outcome at all was accepted.
    -- Which is exactly the state this constraint exists to forbid.
    CONSTRAINT vendor_dd_checks_outcome_requires_conclusion
        CHECK (
            (status = 'STARTED'   AND risk_outcome IS NULL AND completed_at IS NULL)
         OR (status = 'FAILED'    AND risk_outcome IS NULL AND completed_at IS NOT NULL)
         OR (status = 'COMPLETED' AND risk_outcome IS NOT NULL
                                  AND risk_outcome IN ('CLEAR', 'FLAGGED')
                                  AND completed_at IS NOT NULL)
        ),

    -- A concluded check must say what screened it. New here: on the compose
    -- estate the column is nullable for rows that predate it, which this
    -- database has none of.
    CONSTRAINT vendor_dd_checks_concluded_names_source
        CHECK (status = 'STARTED' OR screening_source IS NOT NULL),

    -- A blank vendor name is not a vendor. The service refuses one at the
    -- boundary; this is the backstop for any other writer.
    CONSTRAINT vendor_dd_checks_vendor_name_present
        CHECK (btrim(vendor_name) <> '')
);

-- Idempotency: a retried start resolves to the original check, never a second,
-- divergent one.
CREATE UNIQUE INDEX idx_vendor_dd_checks_tenant_correlation
    ON vendor_due_diligence.vendor_dd_checks (tenant_id, correlation_id);

CREATE INDEX idx_vendor_dd_checks_tenant_entity
    ON vendor_due_diligence.vendor_dd_checks (tenant_id, legal_entity_id);
CREATE INDEX idx_vendor_dd_checks_tenant_counterparty
    ON vendor_due_diligence.vendor_dd_checks (tenant_id, counterparty_id);

-- Composite key the evidence table's foreign key points at, so a piece of
-- evidence cannot be attached to another tenant's check.
CREATE UNIQUE INDEX idx_vendor_dd_checks_id_tenant
    ON vendor_due_diligence.vendor_dd_checks (check_id, tenant_id);

-- ── vendor_dd_evidence ───────────────────────────────────────────────────────

CREATE TABLE vendor_due_diligence.vendor_dd_evidence (
    evidence_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    check_id           UUID         NOT NULL,
    tenant_id          VARCHAR(255) NOT NULL,

    evidence_type      VARCHAR(100) NOT NULL,
    description        TEXT         NOT NULL,

    -- NULL means "no document", not "a document whose reference is blank".
    -- 000001 shipped this column with nothing ever writing to it, so every row
    -- held the empty string and the two cases were indistinguishable. The
    -- constraint keeps them apart from here on.
    document_reference VARCHAR(255),

    recorded_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT vendor_dd_evidence_document_reference_not_blank
        CHECK (document_reference IS NULL OR btrim(document_reference) <> ''),

    -- Composite, so evidence and its check always agree on the tenant.
    CONSTRAINT vendor_dd_evidence_check_fk
        FOREIGN KEY (check_id, tenant_id)
        REFERENCES vendor_due_diligence.vendor_dd_checks (check_id, tenant_id)
        ON DELETE CASCADE
);

CREATE INDEX idx_vendor_dd_evidence_check
    ON vendor_due_diligence.vendor_dd_evidence (check_id);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE vendor_due_diligence.vendor_dd_checks   ENABLE ROW LEVEL SECURITY;
ALTER TABLE vendor_due_diligence.vendor_dd_checks   FORCE  ROW LEVEL SECURITY;
ALTER TABLE vendor_due_diligence.vendor_dd_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE vendor_due_diligence.vendor_dd_evidence FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON vendor_due_diligence.vendor_dd_checks
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON vendor_due_diligence.vendor_dd_checks
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON vendor_due_diligence.vendor_dd_evidence
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON vendor_due_diligence.vendor_dd_evidence
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON vendor_due_diligence.vendor_dd_checks   TO authenticated;
GRANT SELECT ON vendor_due_diligence.vendor_dd_evidence TO authenticated;

GRANT SELECT, INSERT, UPDATE ON vendor_due_diligence.vendor_dd_checks TO zoiko_backend;

-- Evidence is recorded, not revised: a conclusion must not be able to outlive
-- the evidence that supported it.
GRANT SELECT, INSERT ON vendor_due_diligence.vendor_dd_evidence TO zoiko_backend;
