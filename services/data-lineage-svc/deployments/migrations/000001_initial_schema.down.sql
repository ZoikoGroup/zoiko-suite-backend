-- Down migration for data-lineage-svc (DATA-03)
-- Migration: 000001_initial_schema.down.sql

DROP TRIGGER IF EXISTS trigger_evidence_attachment_immutability ON evidence_attachments;
DROP FUNCTION IF EXISTS enforce_evidence_attachment_immutability();

DROP TRIGGER IF EXISTS trigger_transformation_version_immutability ON transformation_versions;
DROP FUNCTION IF EXISTS enforce_transformation_version_immutability();

DROP TRIGGER IF EXISTS trigger_provenance_manifest_immutability ON provenance_manifests;
DROP FUNCTION IF EXISTS enforce_provenance_manifest_immutability();

DROP TRIGGER IF EXISTS trigger_lineage_edge_immutability ON lineage_edges;
DROP FUNCTION IF EXISTS enforce_lineage_edge_immutability();

DROP TRIGGER IF EXISTS trigger_lineage_entity_immutability ON lineage_entities;
DROP FUNCTION IF EXISTS enforce_lineage_entity_immutability();

DROP INDEX IF EXISTS idx_outbox_events_unpublished;
DROP INDEX IF EXISTS idx_evidence_attachments_entity;
DROP INDEX IF EXISTS idx_provenance_manifests_entity;
DROP INDEX IF EXISTS idx_lineage_edges_supersedes;
DROP INDEX IF EXISTS idx_lineage_edges_derived;
DROP INDEX IF EXISTS idx_lineage_edges_source;

DROP POLICY IF EXISTS outbox_events_tenant_isolation ON outbox_events;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
DROP POLICY IF EXISTS evidence_attachments_tenant_isolation ON evidence_attachments;
DROP POLICY IF EXISTS provenance_manifests_delete ON provenance_manifests;
DROP POLICY IF EXISTS provenance_manifests_update ON provenance_manifests;
DROP POLICY IF EXISTS provenance_manifests_insert ON provenance_manifests;
DROP POLICY IF EXISTS provenance_manifests_read ON provenance_manifests;
DROP POLICY IF EXISTS lineage_edges_tenant_isolation ON lineage_edges;
DROP POLICY IF EXISTS lineage_activities_tenant_isolation ON lineage_activities;
DROP POLICY IF EXISTS transformation_versions_tenant_isolation ON transformation_versions;
DROP POLICY IF EXISTS lineage_entities_tenant_isolation ON lineage_entities;

ALTER TABLE outbox_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys DISABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_attachments DISABLE ROW LEVEL SECURITY;
ALTER TABLE provenance_manifests DISABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_edges DISABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_activities DISABLE ROW LEVEL SECURITY;
ALTER TABLE transformation_versions DISABLE ROW LEVEL SECURITY;
ALTER TABLE lineage_entities DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS evidence_attachments;
DROP TABLE IF EXISTS provenance_manifests;
DROP TABLE IF EXISTS lineage_edges;
DROP TABLE IF EXISTS lineage_activities;
DROP TABLE IF EXISTS transformation_versions;
DROP TABLE IF EXISTS lineage_entities;
