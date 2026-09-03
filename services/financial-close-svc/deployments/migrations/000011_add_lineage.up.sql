-- Migration: 000011_add_lineage.up.sql
--
-- ACC-18 (Source-to-Report Traceability): "owns Lineage graph/index and
-- verification results. Must never own: Underlying accounting/business
-- facts." Fuller ownership: "AccountingLineageGraph/index and
-- verification results; does not own underlying business facts." State
-- model: "Projection: Current / Rebuilding / Degraded; historical
-- lineage immutable even if access policy changes."
--
-- lineage_edges is append-only, permanent evidence of one recorded
-- provenance link (e.g. "this accrual recognition produced this GL
-- journal") — never inferred, never guessed. The spec's own negative
-- path, "Lineage service invents inferred source," is satisfied
-- structurally: this table is the ONLY source TraceJournalToSource and
-- VerifyLineageCompleteness ever read from, and nothing in this service
-- ever derives an edge from naming conventions or heuristics. A journal
-- with no row here has NO recorded lineage — reported as an explicit
-- gap, never silently treated as traced.
--
-- lineage_projection_status is a real, if minimal, per-entity projection
-- state: CURRENT/REBUILDING/DEGRADED, verbatim from the spec's own state
-- model. DEGRADED is set whenever a source capability successfully
-- posts a journal but recording that journal's own lineage edge fails —
-- the spec's own negative path, "Projection stale after adjustment,"
-- made a real, visible state rather than a silent gap nobody would ever
-- notice.

CREATE TABLE lineage_edges (
    edge_id           UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,
    from_type             VARCHAR(64) NOT NULL, -- e.g. 'accrual_recognition', 'allocation_run', 'fx_revaluation_run', 'migration_batch'
    from_id                VARCHAR(255) NOT NULL,
    to_type                 VARCHAR(64) NOT NULL, -- e.g. 'journal'
    to_id                    VARCHAR(255) NOT NULL,
    recorded_at              TIMESTAMP WITH TIME ZONE NOT NULL,
    UNIQUE (tenant_id, from_type, from_id, to_type, to_id)
);

CREATE OR REPLACE FUNCTION reject_lineage_edge_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'lineage_edges is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_lineage_edge_update
    BEFORE UPDATE ON lineage_edges
    FOR EACH ROW EXECUTE FUNCTION reject_lineage_edge_mutation();
CREATE TRIGGER trg_reject_lineage_edge_delete
    BEFORE DELETE ON lineage_edges
    FOR EACH ROW EXECUTE FUNCTION reject_lineage_edge_mutation();

CREATE TABLE lineage_projection_status (
    tenant_id          VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,
    status                VARCHAR(20) NOT NULL, -- CURRENT|REBUILDING|DEGRADED
    degraded_reason        TEXT,
    last_rebuilt_at         TIMESTAMP WITH TIME ZONE,
    PRIMARY KEY (tenant_id, legal_entity_id)
);

ALTER TABLE lineage_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_edges FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON lineage_edges
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE lineage_projection_status ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_projection_status FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON lineage_projection_status
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_lineage_edges_to ON lineage_edges (tenant_id, to_type, to_id);
CREATE INDEX idx_lineage_edges_from ON lineage_edges (tenant_id, from_type, from_id);
