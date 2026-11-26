-- Reverses 000002. Data written under the 000002 model (reviews, threshold
-- lifecycle, idempotency records, undelivered outbox events) is discarded.

DROP TABLE IF EXISTS risk_outbox_events;
DROP TABLE IF EXISTS risk_idempotency_records;

DROP TRIGGER IF EXISTS risk_score_assessments_no_delete ON risk_score_assessments;
DROP TRIGGER IF EXISTS risk_factor_breakdowns_immutable ON risk_factor_breakdowns;
DROP TRIGGER IF EXISTS risk_threshold_rules_no_delete ON risk_threshold_rules;
DROP FUNCTION IF EXISTS risk_scoring_forbid_change();

DROP POLICY IF EXISTS risk_score_assessments_tenant_isolation ON risk_score_assessments;
DROP POLICY IF EXISTS risk_factor_breakdowns_tenant_isolation ON risk_factor_breakdowns;
DROP POLICY IF EXISTS risk_threshold_rules_tenant_isolation ON risk_threshold_rules;
-- FORCE on these tables belongs to 000002_force_rls (on main); left in place.
CREATE POLICY risk_score_assessments_tenant_isolation ON risk_score_assessments
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY risk_factor_breakdowns_tenant_isolation ON risk_factor_breakdowns
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY risk_threshold_rules_tenant_isolation ON risk_threshold_rules
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- risk_threshold_rules
DROP INDEX IF EXISTS uq_risk_threshold_one_active_per_category;
DROP INDEX IF EXISTS idx_risk_threshold_effective;
DROP INDEX IF EXISTS idx_risk_threshold_page;
ALTER TABLE risk_threshold_rules
    DROP CONSTRAINT IF EXISTS risk_threshold_status_valid,
    DROP CONSTRAINT IF EXISTS risk_threshold_category_valid,
    DROP CONSTRAINT IF EXISTS risk_threshold_ladder,
    DROP CONSTRAINT IF EXISTS risk_threshold_effective_range,
    DROP CONSTRAINT IF EXISTS risk_threshold_approved_has_approver,
    ADD COLUMN is_active BOOLEAN NOT NULL DEFAULT TRUE;
UPDATE risk_threshold_rules SET is_active = (status = 'ACTIVE');
ALTER TABLE risk_threshold_rules
    ALTER COLUMN high_threshold SET DEFAULT 60.00,
    ALTER COLUMN critical_threshold SET DEFAULT 80.00,
    DROP COLUMN moderate_threshold,
    DROP COLUMN status,
    DROP COLUMN version,
    DROP COLUMN effective_from,
    DROP COLUMN effective_to,
    DROP COLUMN created_by,
    DROP COLUMN updated_by,
    DROP COLUMN approved_by,
    DROP COLUMN approved_at,
    DROP COLUMN retired_by,
    DROP COLUMN retired_at,
    DROP COLUMN supersedes_rule_id,
    DROP COLUMN updated_at;

-- risk_factor_breakdowns
ALTER TABLE risk_factor_breakdowns
    DROP CONSTRAINT IF EXISTS risk_factor_category_valid,
    DROP CONSTRAINT IF EXISTS risk_factor_threshold_band_valid,
    DROP CONSTRAINT IF EXISTS risk_factor_breakdowns_assessment_id_fkey,
    ADD CONSTRAINT risk_factor_breakdowns_assessment_id_fkey
        FOREIGN KEY (assessment_id) REFERENCES risk_score_assessments(id) ON DELETE CASCADE;
UPDATE risk_factor_breakdowns SET category_weight = category_weight / 100 WHERE category_weight > 1;
ALTER TABLE risk_factor_breakdowns
    DROP COLUMN input_count,
    DROP COLUMN saturation_count,
    DROP COLUMN category_threshold_band;

-- risk_score_assessments
DROP INDEX IF EXISTS uq_risk_score_one_active_per_subject;
DROP INDEX IF EXISTS idx_risk_score_subject_history;
DROP INDEX IF EXISTS idx_risk_score_page;
ALTER TABLE risk_score_assessments
    DROP CONSTRAINT IF EXISTS risk_score_status_valid,
    DROP CONSTRAINT IF EXISTS risk_score_tier_valid,
    DROP CONSTRAINT IF EXISTS risk_score_subject_type_valid,
    DROP CONSTRAINT IF EXISTS risk_score_review_status_valid,
    DROP CONSTRAINT IF EXISTS risk_score_threshold_source_valid,
    DROP CONSTRAINT IF EXISTS risk_score_range,
    DROP CONSTRAINT IF EXISTS risk_score_counts_nonneg,
    DROP CONSTRAINT IF EXISTS risk_score_archived_has_time;
UPDATE risk_score_assessments SET status = 'ACTIVE' WHERE status = 'SUPERSEDED';
ALTER TABLE risk_score_assessments
    DROP COLUMN subject_type,
    DROP COLUMN subject_id,
    DROP COLUMN model_version,
    DROP COLUMN input_hash,
    DROP COLUMN threshold_source,
    DROP COLUMN threshold_rule_id,
    DROP COLUMN threshold_rule_version,
    DROP COLUMN moderate_cutoff,
    DROP COLUMN high_cutoff,
    DROP COLUMN critical_cutoff,
    DROP COLUMN band_rationale,
    DROP COLUMN assurance_state,
    DROP COLUMN review_status,
    DROP COLUMN reviewed_by,
    DROP COLUMN reviewed_at,
    DROP COLUMN review_note,
    DROP COLUMN calculated_by,
    DROP COLUMN previous_score_id,
    DROP COLUMN score_delta,
    DROP COLUMN superseded_by_id,
    DROP COLUMN source_references,
    DROP COLUMN version,
    DROP COLUMN updated_at,
    DROP COLUMN archived_at,
    DROP COLUMN archived_by,
    DROP COLUMN archive_reason;
