-- Schema for data-lineage-svc (DATA-03, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Lineage Entities: one addressable thing in the provenance graph.
-- (tenant_id, entity_type, external_ref) is the natural key callers use —
-- RecordDerivation finds-or-creates by this triple so the same real thing
-- never gets a duplicate row across repeated calls.
CREATE TABLE IF NOT EXISTS lineage_entities (
    entity_id           TEXT         PRIMARY KEY
        CHECK (entity_id ~ '^dle_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id            VARCHAR(64)  NOT NULL,
    entity_type           VARCHAR(64)  NOT NULL,
    external_ref            VARCHAR(256) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, entity_type, external_ref)
);

-- Transformation Versions: an immutable registry of named transformation
-- versions (e.g. "data-ingestion-svc:CommitBatch:v1"), auto-registered by
-- RecordDerivation the first time a name is used.
CREATE TABLE IF NOT EXISTS transformation_versions (
    version_id           TEXT         PRIMARY KEY
        CHECK (version_id ~ '^dtv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    name                     VARCHAR(256) NOT NULL,
    registered_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    registered_by                VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, name)
);

-- Lineage Activities: one transformation/derivation event.
-- transformation_version is nullable BY DESIGN — a caller may record a
-- derivation without full evidence; SealManifest is what refuses to
-- certify a manifest whose upstream graph has any activity missing one.
CREATE TABLE IF NOT EXISTS lineage_activities (
    activity_id              TEXT         PRIMARY KEY
        CHECK (activity_id ~ '^dla_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                 VARCHAR(64)  NOT NULL,
    activity_type               VARCHAR(64)  NOT NULL,
    transformation_version         VARCHAR(256),
    agent                             VARCHAR(128) NOT NULL,
    occurred_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Lineage Edges: append-only. SupersedeLineage never mutates an existing
-- edge's source/activity/derived fields — it inserts a NEW edge with
-- supersedes_edge_id pointing at the old one. "Current" is computed (is
-- anything superseding this edge?), never stored as a mutable flag — the
-- doc's own doctrine: "Lineage is append/supersede, not destructive edit."
CREATE TABLE IF NOT EXISTS lineage_edges (
    edge_id                 TEXT         PRIMARY KEY
        CHECK (edge_id ~ '^dlg_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                 VARCHAR(64)  NOT NULL,
    source_entity_id            TEXT         NOT NULL REFERENCES lineage_entities(entity_id),
    activity_id                   TEXT         NOT NULL REFERENCES lineage_activities(activity_id),
    derived_entity_id                TEXT         NOT NULL REFERENCES lineage_entities(entity_id),
    supersedes_edge_id                  TEXT         REFERENCES lineage_edges(edge_id),
    reason                                 TEXT,
    created_at                               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                 VARCHAR(128) NOT NULL,
    CONSTRAINT lineage_edges_reason_required_when_superseding
        CHECK (supersedes_edge_id IS NULL OR reason IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_lineage_edges_source ON lineage_edges(tenant_id, source_entity_id, created_at);
CREATE INDEX IF NOT EXISTS idx_lineage_edges_derived ON lineage_edges(tenant_id, derived_entity_id, created_at);
CREATE INDEX IF NOT EXISTS idx_lineage_edges_supersedes ON lineage_edges(supersedes_edge_id);

-- Provenance Manifests: sealed, immutable snapshots of the full upstream
-- lineage graph for one entity, computed server-side at SealManifest time
-- — same doctrine as commercial-account-svc's CommercialEvidencePackage.
CREATE TABLE IF NOT EXISTS provenance_manifests (
    manifest_id             TEXT         PRIMARY KEY
        CHECK (manifest_id ~ '^dpm_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                 VARCHAR(64)  NOT NULL,
    entity_id                   TEXT         NOT NULL REFERENCES lineage_entities(entity_id),
    manifest                       JSONB        NOT NULL,
    manifest_sha256                   CHAR(64)     NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    sealed_at                           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    sealed_by                             VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_provenance_manifests_entity ON provenance_manifests(tenant_id, entity_id, sealed_at);

-- Evidence Attachments: an external evidence reference attached to a
-- LineageEntity without altering the entity itself. Append-only.
CREATE TABLE IF NOT EXISTS evidence_attachments (
    attachment_id            TEXT         PRIMARY KEY
        CHECK (attachment_id ~ '^dea_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                  VARCHAR(64)  NOT NULL,
    entity_id                    TEXT         NOT NULL REFERENCES lineage_entities(entity_id),
    evidence_ref                    TEXT         NOT NULL,
    attached_at                        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    attached_by                          VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_evidence_attachments_entity ON evidence_attachments(tenant_id, entity_id);

-- Idempotency Keys: for replay-safe writes. Scoped per tenant from the
-- start (data-ingestion-svc's migration originally omitted tenant_id here
-- despite its own RLS policy referencing one — don't repeat that).
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE lineage_entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_entities FORCE ROW LEVEL SECURITY;
CREATE POLICY lineage_entities_tenant_isolation ON lineage_entities
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE transformation_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE transformation_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY transformation_versions_tenant_isolation ON transformation_versions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE lineage_activities ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_activities FORCE ROW LEVEL SECURITY;
CREATE POLICY lineage_activities_tenant_isolation ON lineage_activities
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE lineage_edges ENABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_edges FORCE ROW LEVEL SECURITY;
CREATE POLICY lineage_edges_tenant_isolation ON lineage_edges
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE provenance_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE provenance_manifests FORCE ROW LEVEL SECURITY;
CREATE POLICY provenance_manifests_read ON provenance_manifests FOR SELECT
    USING (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY provenance_manifests_insert ON provenance_manifests FOR INSERT
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
-- UPDATE/DELETE granted too (not merely SELECT/INSERT), so a mutation
-- attempt reaches the immutability trigger below rather than being
-- silently filtered to zero rows by RLS alone.
CREATE POLICY provenance_manifests_update ON provenance_manifests FOR UPDATE
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
CREATE POLICY provenance_manifests_delete ON provenance_manifests FOR DELETE
    USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE evidence_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_attachments FORCE ROW LEVEL SECURITY;
CREATE POLICY evidence_attachments_tenant_isolation ON evidence_attachments
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Immutability ─────────────────────────────────────────────────────────────

-- Lineage entities are never legitimately mutated after creation — the
-- find-or-create lookup means there is no update path at all.
CREATE OR REPLACE FUNCTION enforce_lineage_entity_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'lineage entity is immutable once created';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_lineage_entity_immutability
    BEFORE UPDATE OR DELETE ON lineage_entities
    FOR EACH ROW EXECUTE FUNCTION enforce_lineage_entity_immutability();

-- Lineage edges are append-only: never updated, never deleted.
CREATE OR REPLACE FUNCTION enforce_lineage_edge_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'lineage edge is immutable (append/supersede, not destructive edit)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_lineage_edge_immutability
    BEFORE UPDATE OR DELETE ON lineage_edges
    FOR EACH ROW EXECUTE FUNCTION enforce_lineage_edge_immutability();

-- Sealed provenance manifests are immutable — same reject-on-mutation
-- doctrine as every other sealed evidence table in this repo.
CREATE OR REPLACE FUNCTION enforce_provenance_manifest_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'provenance manifest is sealed and immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_provenance_manifest_immutability
    BEFORE UPDATE OR DELETE ON provenance_manifests
    FOR EACH ROW EXECUTE FUNCTION enforce_provenance_manifest_immutability();

-- Transformation versions are an append-only registry.
CREATE OR REPLACE FUNCTION enforce_transformation_version_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'transformation version is immutable once registered';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_transformation_version_immutability
    BEFORE UPDATE OR DELETE ON transformation_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_transformation_version_immutability();

-- Evidence attachments are append-only.
CREATE OR REPLACE FUNCTION enforce_evidence_attachment_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'evidence attachment is immutable once written';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_evidence_attachment_immutability
    BEFORE UPDATE OR DELETE ON evidence_attachments
    FOR EACH ROW EXECUTE FUNCTION enforce_evidence_attachment_immutability();
