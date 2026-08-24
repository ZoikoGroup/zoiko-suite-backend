-- 000003_add_rls.down.sql
--
-- Child before parent, mirroring the up migration.
--
-- Per the caveat in the up file: a PARTIAL hand-rollback that only disables
-- RLS on evidence_manifests would widen manifest_records to every tenant's
-- evidence, because its policy resolves through the parent. This file
-- removes both, so that asymmetry does not arise here — but it is the
-- reason not to reach for a one-line ALTER TABLE when debugging.

DROP POLICY IF EXISTS tenant_isolation_policy ON manifest_records;
ALTER TABLE manifest_records NO FORCE ROW LEVEL SECURITY;
ALTER TABLE manifest_records DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON evidence_manifests;
ALTER TABLE evidence_manifests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE evidence_manifests DISABLE ROW LEVEL SECURITY;
