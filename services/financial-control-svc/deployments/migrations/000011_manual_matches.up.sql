-- SPEC_DEVIATIONS A5: matching was fully automatic — there was no command
-- to manually pair two records with permission, reason and evidence (§10
-- "Manual match: requires permission, reason and evidence; cannot hide
-- amount difference"). A separate table from match_results: a manual match
-- is a human decision, not an algorithm result, and match_results' CHECK
-- (outcome IN ('EXACT','WITHIN_TOLERANCE')) and its MatchAlgorithmVersion
-- provenance have no honest meaning for one.

CREATE TABLE manual_matches (
    manual_match_id   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    run_id            UUID NOT NULL REFERENCES control_runs(run_id),
    side_a_record_ids TEXT[] NOT NULL CHECK (cardinality(side_a_record_ids) > 0),
    side_b_record_ids TEXT[] NOT NULL CHECK (cardinality(side_b_record_ids) > 0),
    amount_a          NUMERIC(38,12) NOT NULL,
    amount_b          NUMERIC(38,12) NOT NULL,
    difference        NUMERIC(38,12) NOT NULL,
    currency          CHAR(3) NOT NULL,
    reason            TEXT NOT NULL CHECK (reason <> ''),
    evidence_ref      TEXT NOT NULL CHECK (evidence_ref <> ''),
    created_by        VARCHAR(255) NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_manual_matches_run ON manual_matches (tenant_id, run_id);

-- Append-only, like every other evidential row this service writes (000001).
CREATE TRIGGER trg_immutable_manual_matches BEFORE UPDATE OR DELETE ON manual_matches
    FOR EACH ROW EXECUTE FUNCTION reject_control_mutation();

ALTER TABLE manual_matches ENABLE ROW LEVEL SECURITY;
ALTER TABLE manual_matches FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON manual_matches FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
