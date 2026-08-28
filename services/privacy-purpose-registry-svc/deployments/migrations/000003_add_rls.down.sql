DROP POLICY IF EXISTS tenant_isolation_policy ON processing_activity_versions;
ALTER TABLE processing_activity_versions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON processing_activities;
ALTER TABLE processing_activities DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON purpose_versions;
ALTER TABLE purpose_versions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON purposes;
ALTER TABLE purposes DISABLE ROW LEVEL SECURITY;
