-- ZS-SVC-Z-001 section 11 (docs/architecture/DQC_GAP_MATRIX.md, step F4): the /dqc/* routes.
--
--   dq_observations         POST /dqc/observations: "Idempotency key required per asset + window + source
--                           watermark". An observation is the recorded fact that an asset's data was seen over a
--                           window at a source watermark, with the evidence hash of what was seen. It is append-only:
--                           late-arriving evidence is a NEW observation (a later watermark), never a change to an
--                           earlier one (section 12).
--   dq_runs.evidence_fingerprint
--                           POST /dqc/validate: "Same pinned asset/contract/evidence returns same logical result".
--                           A hash of exactly what a contract run was judged on, unique per tenant, so the same
--                           evidence cannot be recorded as two runs.
--   uq_dq_issues_result     POST /dqc/exceptions: "Dedup key prevents alert storms for same control/root
--                           evidence". One exception per failing result.

CREATE TABLE IF NOT EXISTS dq_observations (
    observation_run_id  TEXT         PRIMARY KEY
        CHECK (observation_run_id ~ '^dqo_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id           VARCHAR(64)  NOT NULL,
    asset_id            TEXT         NOT NULL REFERENCES dqc_assets(asset_id),
    window_start        TIMESTAMPTZ  NOT NULL,
    window_end          TIMESTAMPTZ  NOT NULL,
    source_watermark    VARCHAR(256) NOT NULL CHECK (length(btrim(source_watermark)) > 0),
    source_system       VARCHAR(128) NOT NULL CHECK (length(btrim(source_system)) > 0),
    row_count           BIGINT       NOT NULL CHECK (row_count >= 0),
    evidence_hash       CHAR(64)     NOT NULL CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    recorded_by         VARCHAR(128) NOT NULL,
    recorded_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT dq_observation_window_ordered CHECK (window_end > window_start),
    UNIQUE (tenant_id, asset_id, window_start, window_end, source_watermark)
);

CREATE OR REPLACE FUNCTION enforce_dq_observation_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dq observation is immutable: late-arriving evidence is a new observation at a later watermark';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dq_observation_immutability ON dq_observations;
CREATE TRIGGER trigger_dq_observation_immutability
    BEFORE UPDATE OR DELETE ON dq_observations
    FOR EACH ROW EXECUTE FUNCTION enforce_dq_observation_immutability();

ALTER TABLE dq_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE dq_observations FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dq_observations_tenant_isolation ON dq_observations;
CREATE POLICY dq_observations_tenant_isolation ON dq_observations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Same evidence, one run. Existing runs carry no fingerprint (nothing is backfilled).
ALTER TABLE dq_runs ADD COLUMN IF NOT EXISTS evidence_fingerprint CHAR(64);
ALTER TABLE dq_runs DROP CONSTRAINT IF EXISTS dq_runs_fingerprint_format;
ALTER TABLE dq_runs ADD CONSTRAINT dq_runs_fingerprint_format
    CHECK (evidence_fingerprint IS NULL OR evidence_fingerprint ~ '^[0-9a-f]{64}$');
CREATE UNIQUE INDEX IF NOT EXISTS uq_dq_runs_evidence_fingerprint
    ON dq_runs (tenant_id, evidence_fingerprint) WHERE evidence_fingerprint IS NOT NULL;

-- One exception per failing result. Refuses, rather than skips, if history already holds duplicates:
-- an index that silently was not created would leave the guarantee unenforced.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM dq_issues GROUP BY tenant_id, result_id HAVING count(*) > 1) THEN
        RAISE EXCEPTION '000007: dq_issues already holds more than one issue for the same result; merge or close the duplicates first';
    END IF;
END
$$;
CREATE UNIQUE INDEX IF NOT EXISTS uq_dq_issues_result ON dq_issues (tenant_id, result_id);
