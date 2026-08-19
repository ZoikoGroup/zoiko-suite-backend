-- 0016_financial_close_svc.sql
-- financial-close-svc → schema `financial_close`
--
-- End state of 000001_initial_schema (the service's only migration).
-- Two tables: fiscal_periods, close_evidences.

CREATE SCHEMA IF NOT EXISTS financial_close;

COMMENT ON SCHEMA financial_close IS
    'financial-close-svc. Fiscal periods and their close state, with the signed trial-balance evidence produced at close.';

GRANT USAGE ON SCHEMA financial_close TO zoiko_backend, authenticated;

-- ── fiscal_periods ───────────────────────────────────────────────────────────

CREATE TABLE financial_close.fiscal_periods (
    fiscal_period_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,

    period_name          VARCHAR(50)  NOT NULL,
    period_start         TIMESTAMPTZ  NOT NULL,
    period_end           TIMESTAMPTZ  NOT NULL,

    -- OPEN | CLOSED | LOCKED
    close_status         VARCHAR(50)  NOT NULL,
    close_locked_at      TIMESTAMPTZ,

    evidence_document_id TEXT,

    UNIQUE (tenant_id, legal_entity_id, period_name),

    CONSTRAINT fiscal_periods_status_known
        CHECK (close_status IN ('OPEN', 'CLOSED', 'LOCKED')),

    CONSTRAINT fiscal_periods_period_ordered
        CHECK (period_end > period_start),

    -- A locked period must say when it was locked, and an open one must not
    -- claim to have been. Written as an equivalence so a stale close_locked_at
    -- cannot linger on a period later reopened.
    CONSTRAINT fiscal_periods_locked_has_timestamp
        CHECK ((close_status = 'LOCKED') = (close_locked_at IS NOT NULL))
);

CREATE INDEX idx_fiscal_periods_tenant_entity
    ON financial_close.fiscal_periods (tenant_id, legal_entity_id);

-- Composite key close_evidences points at, so evidence and its period always
-- agree on the tenant.
CREATE UNIQUE INDEX idx_fiscal_periods_id_tenant
    ON financial_close.fiscal_periods (fiscal_period_id, tenant_id);

-- ── close_evidences ──────────────────────────────────────────────────────────
-- The signed trial balance produced when a period closes. This is the artefact
-- an auditor is handed, so it is append-only: a close whose evidence can be
-- regenerated after the fact evidences nothing.

CREATE TABLE financial_close.close_evidences (
    evidence_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          VARCHAR(255) NOT NULL,
    fiscal_period_id   UUID         NOT NULL,

    trial_balance_hash VARCHAR(255) NOT NULL,
    signature          VARCHAR(255) NOT NULL,
    generated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- A hash and a signature that are present but empty are not evidence; they
    -- are the shape of evidence.
    CONSTRAINT close_evidences_hash_present
        CHECK (btrim(trial_balance_hash) <> ''),
    CONSTRAINT close_evidences_signature_present
        CHECK (btrim(signature) <> ''),

    CONSTRAINT close_evidences_period_fk
        FOREIGN KEY (fiscal_period_id, tenant_id)
        REFERENCES financial_close.fiscal_periods (fiscal_period_id, tenant_id)
        ON DELETE CASCADE
);

CREATE INDEX idx_close_evidences_tenant
    ON financial_close.close_evidences (tenant_id);
CREATE INDEX idx_close_evidences_period
    ON financial_close.close_evidences (fiscal_period_id, generated_at DESC);

CREATE TRIGGER close_evidences_immutable
    BEFORE UPDATE OR DELETE ON financial_close.close_evidences
    FOR EACH ROW EXECUTE FUNCTION app.reject_mutation();

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE financial_close.fiscal_periods  ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_close.fiscal_periods  FORCE  ROW LEVEL SECURITY;
ALTER TABLE financial_close.close_evidences ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_close.close_evidences FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON financial_close.fiscal_periods
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON financial_close.fiscal_periods
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON financial_close.close_evidences
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON financial_close.close_evidences
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON financial_close.fiscal_periods  TO authenticated;
GRANT SELECT ON financial_close.close_evidences TO authenticated;

-- Periods transition OPEN → CLOSED → LOCKED, so UPDATE is granted. There is no
-- DELETE: a period is locked, never removed.
GRANT SELECT, INSERT, UPDATE ON financial_close.fiscal_periods  TO zoiko_backend;
GRANT SELECT, INSERT         ON financial_close.close_evidences TO zoiko_backend;
