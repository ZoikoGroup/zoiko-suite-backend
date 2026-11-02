-- Migration 000002: explainable, versioned, tenant-isolated risk scoring.
--
-- What this adds on top of 000001 (see ROUTES.md "Scoring model" and
-- "State machines" for the behaviour these columns support):
--
--   risk_score_assessments
--     * subject (subject_type, subject_id) so scores have a per-subject history
--     * model_version + input_hash   -> reproducibility of every score
--     * the threshold cut-offs that were applied (and which rule supplied them)
--     * advisory review workflow (review_status, reviewer, note)
--     * score history links (previous_score_id, score_delta, superseded_by_id)
--     * optimistic-concurrency version + audit columns
--     * DELETE is blocked by trigger: historical scores are never removed,
--       they are ARCHIVED.
--
--   risk_factor_breakdowns
--     * input_count / saturation_count / category threshold band
--     * category_weight is now stored as a percentage (30.00 = 30 %)
--     * rows are immutable (UPDATE and DELETE blocked by trigger); the
--       ON DELETE CASCADE from 000001 is replaced by RESTRICT
--
--   risk_threshold_rules
--     * maker-checker lifecycle DRAFT -> ACTIVE -> SUPERSEDED / RETIRED
--     * effective dating (effective_from / effective_to)
--     * moderate threshold so a rule defines the full band ladder
--     * is_active is replaced by status (single source of truth)
--
--   risk_idempotency_records  -> Idempotency-Key replay protection (INV-08)
--   risk_outbox_events        -> transactional outbox for domain events
--
-- RLS: every tenant table gets FORCE ROW LEVEL SECURITY and a policy with
-- both USING and WITH CHECK. NULLIF(...,'') makes an empty app.tenant_id
-- match nothing instead of matching tenant ''.

-- ---------------------------------------------------------------------------
-- risk_score_assessments
-- ---------------------------------------------------------------------------
ALTER TABLE risk_score_assessments
    ADD COLUMN subject_type          VARCHAR(32),
    ADD COLUMN subject_id            VARCHAR(128),
    ADD COLUMN model_version         VARCHAR(64),
    ADD COLUMN input_hash            VARCHAR(80),
    ADD COLUMN threshold_source      VARCHAR(32),
    ADD COLUMN threshold_rule_id     UUID,
    ADD COLUMN threshold_rule_version INT,
    ADD COLUMN moderate_cutoff       NUMERIC(5,2),
    ADD COLUMN high_cutoff           NUMERIC(5,2),
    ADD COLUMN critical_cutoff       NUMERIC(5,2),
    ADD COLUMN band_rationale        TEXT,
    ADD COLUMN assurance_state       VARCHAR(32) NOT NULL DEFAULT 'ADVISORY',
    ADD COLUMN review_status         VARCHAR(32),
    ADD COLUMN reviewed_by           VARCHAR(128),
    ADD COLUMN reviewed_at           TIMESTAMPTZ,
    ADD COLUMN review_note           TEXT,
    ADD COLUMN calculated_by         VARCHAR(128),
    ADD COLUMN previous_score_id     UUID,
    ADD COLUMN score_delta           NUMERIC(6,2),
    ADD COLUMN superseded_by_id      UUID,
    ADD COLUMN source_references     JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN version               INT NOT NULL DEFAULT 1,
    ADD COLUMN updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN archived_at           TIMESTAMPTZ,
    ADD COLUMN archived_by           VARCHAR(128),
    ADD COLUMN archive_reason        TEXT;

-- Backfill rows written by the 000001-era code: they scored the legal entity
-- with the same five-factor model (now named CRS-WEIGHTED-1.0) against the
-- hard-coded bands 26/51/76.
UPDATE risk_score_assessments SET
    subject_type     = 'LEGAL_ENTITY',
    subject_id       = legal_entity_id,
    model_version    = 'CRS-WEIGHTED-1.0',
    input_hash       = 'legacy',
    threshold_source = 'PLATFORM_DEFAULT',
    moderate_cutoff  = 26.00,
    high_cutoff      = 51.00,
    critical_cutoff  = 76.00,
    band_rationale   = 'legacy score (pre-000002); platform default bands',
    review_status    = CASE WHEN risk_tier IN ('HIGH','CRITICAL') THEN 'PENDING_REVIEW' ELSE 'NOT_REQUIRED' END,
    calculated_by    = 'unknown-legacy';

-- Only one ACTIVE score may exist per subject; older ones become SUPERSEDED.
WITH ranked AS (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY tenant_id, subject_type, subject_id
                                  ORDER BY evaluated_at DESC, id DESC) AS rn
    FROM risk_score_assessments WHERE status = 'ACTIVE'
)
UPDATE risk_score_assessments a SET status = 'SUPERSEDED'
FROM ranked r WHERE a.id = r.id AND r.rn > 1;

ALTER TABLE risk_score_assessments
    ALTER COLUMN subject_type     SET NOT NULL,
    ALTER COLUMN subject_id       SET NOT NULL,
    ALTER COLUMN model_version    SET NOT NULL,
    ALTER COLUMN input_hash       SET NOT NULL,
    ALTER COLUMN threshold_source SET NOT NULL,
    ALTER COLUMN moderate_cutoff  SET NOT NULL,
    ALTER COLUMN high_cutoff      SET NOT NULL,
    ALTER COLUMN critical_cutoff  SET NOT NULL,
    ALTER COLUMN band_rationale   SET NOT NULL,
    ALTER COLUMN review_status    SET NOT NULL,
    ALTER COLUMN calculated_by    SET NOT NULL;

ALTER TABLE risk_score_assessments
    ADD CONSTRAINT risk_score_status_valid
        CHECK (status IN ('ACTIVE', 'SUPERSEDED', 'ARCHIVED')),
    ADD CONSTRAINT risk_score_tier_valid
        CHECK (risk_tier IN ('LOW', 'MODERATE', 'HIGH', 'CRITICAL')),
    ADD CONSTRAINT risk_score_subject_type_valid
        CHECK (subject_type IN ('LEGAL_ENTITY', 'OBLIGATION', 'FILING', 'CONTROL')),
    ADD CONSTRAINT risk_score_review_status_valid
        CHECK (review_status IN ('NOT_REQUIRED', 'PENDING_REVIEW', 'CONFIRMED', 'DISMISSED', 'ESCALATED')),
    ADD CONSTRAINT risk_score_threshold_source_valid
        CHECK (threshold_source IN ('PLATFORM_DEFAULT', 'TENANT_RULE')),
    ADD CONSTRAINT risk_score_range
        CHECK (composite_risk_score >= 0 AND composite_risk_score <= 100),
    ADD CONSTRAINT risk_score_counts_nonneg
        CHECK (open_obligations_count >= 0 AND policy_violations_count >= 0 AND
               audit_exceptions_count >= 0 AND privacy_incidents_count >= 0 AND
               tax_penalties_count >= 0),
    ADD CONSTRAINT risk_score_archived_has_time
        CHECK (status <> 'ARCHIVED' OR archived_at IS NOT NULL);

CREATE UNIQUE INDEX IF NOT EXISTS uq_risk_score_one_active_per_subject
    ON risk_score_assessments (tenant_id, subject_type, subject_id)
    WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_risk_score_subject_history
    ON risk_score_assessments (tenant_id, subject_type, subject_id, evaluated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_risk_score_page
    ON risk_score_assessments (tenant_id, created_at DESC, id DESC);

-- ---------------------------------------------------------------------------
-- risk_factor_breakdowns
-- ---------------------------------------------------------------------------
ALTER TABLE risk_factor_breakdowns
    ADD COLUMN input_count             INT,
    ADD COLUMN saturation_count        INT,
    ADD COLUMN category_threshold_band VARCHAR(32);

-- 000001-era code stored the weight as a fraction (0.30); store percentages.
UPDATE risk_factor_breakdowns SET category_weight = category_weight * 100 WHERE category_weight <= 1;

ALTER TABLE risk_factor_breakdowns
    DROP CONSTRAINT IF EXISTS risk_factor_breakdowns_assessment_id_fkey,
    ADD CONSTRAINT risk_factor_breakdowns_assessment_id_fkey
        FOREIGN KEY (assessment_id) REFERENCES risk_score_assessments(id) ON DELETE RESTRICT,
    ADD CONSTRAINT risk_factor_category_valid
        CHECK (risk_category IN ('REGULATORY_OBLIGATIONS', 'POLICY_VIOLATIONS', 'AUDIT_EXCEPTIONS',
                                 'DATA_PRIVACY', 'TAX_COMPLIANCE')),
    ADD CONSTRAINT risk_factor_threshold_band_valid
        CHECK (category_threshold_band IS NULL OR category_threshold_band IN ('MODERATE', 'HIGH', 'CRITICAL'));

-- ---------------------------------------------------------------------------
-- risk_threshold_rules
-- ---------------------------------------------------------------------------
ALTER TABLE risk_threshold_rules
    ADD COLUMN moderate_threshold NUMERIC(5,2),
    ADD COLUMN status             VARCHAR(32),
    ADD COLUMN version            INT NOT NULL DEFAULT 1,
    ADD COLUMN effective_from     DATE,
    ADD COLUMN effective_to       DATE,
    ADD COLUMN created_by         VARCHAR(128),
    ADD COLUMN updated_by         VARCHAR(128),
    ADD COLUMN approved_by        VARCHAR(128),
    ADD COLUMN approved_at        TIMESTAMPTZ,
    ADD COLUMN retired_by         VARCHAR(128),
    ADD COLUMN retired_at         TIMESTAMPTZ,
    ADD COLUMN supersedes_rule_id UUID,
    ADD COLUMN updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Backfill: 000001 rules had no approval step and were live on creation.
-- Keep the newest active rule per category ACTIVE, older ones SUPERSEDED,
-- inactive ones RETIRED. Their approval is recorded as 'legacy-unapproved'.
UPDATE risk_threshold_rules SET
    moderate_threshold = LEAST(26.00, high_threshold - 1),
    status             = CASE WHEN is_active THEN 'ACTIVE' ELSE 'RETIRED' END,
    effective_from     = created_at::date,
    created_by         = 'unknown-legacy',
    updated_by         = 'unknown-legacy',
    approved_by        = 'legacy-unapproved',
    approved_at        = created_at;

WITH ranked AS (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY tenant_id, risk_category ORDER BY created_at DESC, id DESC) AS rn
    FROM risk_threshold_rules WHERE status = 'ACTIVE'
)
UPDATE risk_threshold_rules t SET status = 'SUPERSEDED', effective_to = t.effective_from
FROM ranked r WHERE t.id = r.id AND r.rn > 1;

ALTER TABLE risk_threshold_rules
    ALTER COLUMN moderate_threshold SET NOT NULL,
    ALTER COLUMN status             SET NOT NULL,
    ALTER COLUMN effective_from     SET NOT NULL,
    ALTER COLUMN created_by         SET NOT NULL,
    ALTER COLUMN updated_by         SET NOT NULL,
    ALTER COLUMN high_threshold     DROP DEFAULT,
    ALTER COLUMN critical_threshold DROP DEFAULT,
    DROP COLUMN is_active;

ALTER TABLE risk_threshold_rules
    ADD CONSTRAINT risk_threshold_status_valid
        CHECK (status IN ('DRAFT', 'ACTIVE', 'SUPERSEDED', 'RETIRED')),
    ADD CONSTRAINT risk_threshold_category_valid
        CHECK (risk_category IN ('COMPOSITE', 'REGULATORY_OBLIGATIONS', 'POLICY_VIOLATIONS',
                                 'AUDIT_EXCEPTIONS', 'DATA_PRIVACY', 'TAX_COMPLIANCE')),
    ADD CONSTRAINT risk_threshold_ladder
        CHECK (moderate_threshold > 0 AND moderate_threshold < high_threshold
               AND high_threshold < critical_threshold AND critical_threshold <= 100),
    ADD CONSTRAINT risk_threshold_effective_range
        CHECK (effective_to IS NULL OR effective_to >= effective_from),
    ADD CONSTRAINT risk_threshold_approved_has_approver
        CHECK (status = 'DRAFT' OR approved_at IS NOT NULL);

CREATE UNIQUE INDEX IF NOT EXISTS uq_risk_threshold_one_active_per_category
    ON risk_threshold_rules (tenant_id, risk_category) WHERE status = 'ACTIVE';
CREATE INDEX IF NOT EXISTS idx_risk_threshold_effective
    ON risk_threshold_rules (tenant_id, risk_category, effective_from DESC);
CREATE INDEX IF NOT EXISTS idx_risk_threshold_page
    ON risk_threshold_rules (tenant_id, created_at DESC, id DESC);

-- ---------------------------------------------------------------------------
-- History protection triggers
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION risk_scoring_forbid_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'compliance-risk-scoring: % on % is not permitted (historical scoring evidence)',
        TG_OP, TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS risk_score_assessments_no_delete ON risk_score_assessments;
CREATE TRIGGER risk_score_assessments_no_delete
    BEFORE DELETE ON risk_score_assessments
    FOR EACH ROW EXECUTE FUNCTION risk_scoring_forbid_change();

DROP TRIGGER IF EXISTS risk_factor_breakdowns_immutable ON risk_factor_breakdowns;
CREATE TRIGGER risk_factor_breakdowns_immutable
    BEFORE UPDATE OR DELETE ON risk_factor_breakdowns
    FOR EACH ROW EXECUTE FUNCTION risk_scoring_forbid_change();

DROP TRIGGER IF EXISTS risk_threshold_rules_no_delete ON risk_threshold_rules;
CREATE TRIGGER risk_threshold_rules_no_delete
    BEFORE DELETE ON risk_threshold_rules
    FOR EACH ROW EXECUTE FUNCTION risk_scoring_forbid_change();

-- ---------------------------------------------------------------------------
-- Idempotency records (INV-08)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS risk_idempotency_records (
    tenant_id       VARCHAR(64)  NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL,
    operation       VARCHAR(64)  NOT NULL,
    request_hash    VARCHAR(80)  NOT NULL,
    response_status INT          NOT NULL,
    response_body   JSON         NOT NULL, -- JSON (not JSONB) keeps the exact bytes replayed
    response_etag   VARCHAR(64),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, idempotency_key)
);

-- ---------------------------------------------------------------------------
-- Transactional outbox. Deliberately NOT under RLS: only the in-process
-- relay reads it, across all tenants, and it is never exposed over HTTP.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS risk_outbox_events (
    id             UUID PRIMARY KEY,
    tenant_id      VARCHAR(64)  NOT NULL,
    event_type     VARCHAR(128) NOT NULL,
    aggregate_type VARCHAR(64)  NOT NULL,
    aggregate_id   VARCHAR(128) NOT NULL,
    envelope       JSONB        NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at   TIMESTAMPTZ,
    attempts       INT          NOT NULL DEFAULT 0,
    last_error     TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_risk_outbox_pending
    ON risk_outbox_events (next_attempt_at, created_at) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- Row-level security: FORCE + USING + WITH CHECK on every tenant table.
-- ---------------------------------------------------------------------------
ALTER TABLE risk_score_assessments  FORCE ROW LEVEL SECURITY;
ALTER TABLE risk_factor_breakdowns  FORCE ROW LEVEL SECURITY;
ALTER TABLE risk_threshold_rules    FORCE ROW LEVEL SECURITY;
ALTER TABLE risk_idempotency_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_idempotency_records FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS risk_score_assessments_tenant_isolation ON risk_score_assessments;
DROP POLICY IF EXISTS risk_factor_breakdowns_tenant_isolation ON risk_factor_breakdowns;
DROP POLICY IF EXISTS risk_threshold_rules_tenant_isolation ON risk_threshold_rules;
DROP POLICY IF EXISTS risk_idempotency_records_tenant_isolation ON risk_idempotency_records;

CREATE POLICY risk_score_assessments_tenant_isolation ON risk_score_assessments FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY risk_factor_breakdowns_tenant_isolation ON risk_factor_breakdowns FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY risk_threshold_rules_tenant_isolation ON risk_threshold_rules FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY risk_idempotency_records_tenant_isolation ON risk_idempotency_records FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
