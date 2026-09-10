-- Migration: 000014_add_lineage_verification.up.sql
--
-- ACC-18 (Source-to-Report Traceability) gap closure. Migration 000011
-- built the projection (lineage_edges) and its own health state
-- (lineage_projection_status) but never the two commands the spec's own
-- wireframe names alongside RebuildLineageProjection: VerifyTracePath and
-- QuarantineBrokenLineage. The spec's own fuller ownership line —
-- "AccountingLineageGraph/index and verification results" — names
-- verification results as something this capability owns; until this
-- migration nothing persisted one.
--
-- lineage_trace_verifications is append-only permanent evidence that a
-- specific trace path (one from/to edge) was checked, and what the
-- result was — a real audit trail, not a fire-and-forget check.
--
-- lineage_quarantined_gaps is ACC-18's own record of a gap
-- VerifyLineageCompleteness/RebuildLineageProjection would otherwise keep
-- reporting forever, deliberately accepted as known (e.g. a source
-- capability that predates lineage recording, or a gap under active
-- investigation) rather than fixed. QuarantineBrokenLineage is the only
-- way a gap stops appearing in buildCompletenessReport's own Gaps list —
-- there is no other suppression path, so a quarantine is always a real,
-- evidenced, reasoned decision, never a silent exclusion.
CREATE TABLE lineage_trace_verifications (
    verification_id           UUID PRIMARY KEY,
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id               VARCHAR(255) NOT NULL,
    from_type                      VARCHAR(64) NOT NULL,
    from_id                         VARCHAR(255) NOT NULL,
    to_type                          VARCHAR(64) NOT NULL,
    to_id                             VARCHAR(255) NOT NULL,
    verified                          BOOLEAN NOT NULL,
    verified_at                       TIMESTAMP WITH TIME ZONE NOT NULL,
    verified_by_principal_id          VARCHAR(255) NOT NULL
);

CREATE TABLE lineage_quarantined_gaps (
    quarantine_id              UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id                VARCHAR(255) NOT NULL,
    from_type                       VARCHAR(64) NOT NULL,
    from_id                          VARCHAR(255) NOT NULL,
    to_type                           VARCHAR(64) NOT NULL,
    to_id                              VARCHAR(255) NOT NULL,
    reason                             TEXT NOT NULL,
    quarantined_at                     TIMESTAMP WITH TIME ZONE NOT NULL,
    quarantined_by_principal_id        VARCHAR(255) NOT NULL,
    UNIQUE (tenant_id, from_type, from_id, to_type, to_id)
);

CREATE OR REPLACE FUNCTION reject_lineage_verification_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_verification_update
    BEFORE UPDATE ON lineage_trace_verifications
    FOR EACH ROW EXECUTE FUNCTION reject_lineage_verification_mutation();
CREATE TRIGGER trg_reject_verification_delete
    BEFORE DELETE ON lineage_trace_verifications
    FOR EACH ROW EXECUTE FUNCTION reject_lineage_verification_mutation();

CREATE TRIGGER trg_reject_quarantine_update
    BEFORE UPDATE ON lineage_quarantined_gaps
    FOR EACH ROW EXECUTE FUNCTION reject_lineage_verification_mutation();
CREATE TRIGGER trg_reject_quarantine_delete
    BEFORE DELETE ON lineage_quarantined_gaps
    FOR EACH ROW EXECUTE FUNCTION reject_lineage_verification_mutation();

ALTER TABLE lineage_trace_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_trace_verifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON lineage_trace_verifications
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE lineage_quarantined_gaps ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_quarantined_gaps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON lineage_quarantined_gaps
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_lineage_verifications_entity ON lineage_trace_verifications (tenant_id, legal_entity_id);
CREATE INDEX idx_lineage_quarantined_gaps_entity ON lineage_quarantined_gaps (tenant_id, legal_entity_id);
CREATE INDEX idx_lineage_edges_recorded_at ON lineage_edges (tenant_id, to_type, to_id, recorded_at);
