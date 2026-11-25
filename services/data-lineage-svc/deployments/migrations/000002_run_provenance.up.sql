-- ZS-SVC-Z-001 DQC-04 / TC-09 / NP-31..NP-34 (docs/architecture/DQC_GAP_MATRIX.md, step B).
--
-- An edge used to say only "source -> activity -> derived". It could not say
-- WHICH run produced the derived thing, which build and configuration ran, or
-- which exact version of the source and output it was talking about. All of
-- these are nullable BY DESIGN, exactly like transformation_version in 000001:
-- a caller may record a derivation with partial evidence, and SealManifest is
-- what refuses to certify a graph that has any gap. That keeps recording
-- non-breaking while making a certified manifest mean something.

-- Run + code/config provenance lives on the ACTIVITY (one execution).
ALTER TABLE lineage_activities
    ADD COLUMN IF NOT EXISTS run_id         VARCHAR(128),
    ADD COLUMN IF NOT EXISTS build_version  VARCHAR(256),
    ADD COLUMN IF NOT EXISTS config_digest  CHAR(64),
    ADD COLUMN IF NOT EXISTS environment    VARCHAR(64),
    ADD COLUMN IF NOT EXISTS region         VARCHAR(64);

ALTER TABLE lineage_activities
    ADD CONSTRAINT lineage_activities_config_digest_format
    CHECK (config_digest IS NULL OR config_digest ~ '^[0-9a-f]{64}$');

-- Exact input/output versions live on the EDGE (one source -> derived link).
ALTER TABLE lineage_edges
    ADD COLUMN IF NOT EXISTS source_version  VARCHAR(256),
    ADD COLUMN IF NOT EXISTS derived_version VARCHAR(256);

-- NP-32: a pin that names a moving target is not a pin. "latest" tomorrow is a
-- different dataset from "latest" today, so an edge to it proves nothing.
-- The application refuses these too; this is the backstop for any other writer.
ALTER TABLE lineage_edges
    ADD CONSTRAINT lineage_edges_version_pins_not_mutable
    CHECK (
        (source_version  IS NULL OR lower(btrim(source_version))  NOT IN
            ('latest', 'head', 'current', 'main', 'master', 'tip', 'trunk', '@latest', 'live', 'default', ''))
        AND
        (derived_version IS NULL OR lower(btrim(derived_version)) NOT IN
            ('latest', 'head', 'current', 'main', 'master', 'tip', 'trunk', '@latest', 'live', 'default', ''))
    );

-- Activities carried RLS but, unlike edges, entities and manifests, no
-- immutability trigger: a row could be UPDATEd after the fact, rewriting which
-- run, build or configuration produced a fact (NP-39). Nothing in the service
-- updates or deletes an activity, so seal them like the rest of the graph.
-- Correcting provenance is done by SupersedeLineage with a replacement
-- activity, never by editing history.
CREATE OR REPLACE FUNCTION enforce_lineage_activity_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'lineage activity is immutable (supersede the edge with a replacement activity, do not edit history)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_lineage_activity_immutability
    BEFORE UPDATE OR DELETE ON lineage_activities
    FOR EACH ROW EXECUTE FUNCTION enforce_lineage_activity_immutability();

CREATE INDEX IF NOT EXISTS idx_lineage_activities_run ON lineage_activities(tenant_id, run_id) WHERE run_id IS NOT NULL;
