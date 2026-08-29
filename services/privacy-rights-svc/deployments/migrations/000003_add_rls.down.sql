DROP POLICY IF EXISTS tenant_isolation_policy ON discovery_manifests;
ALTER TABLE discovery_manifests DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON identity_verification_events;
ALTER TABLE identity_verification_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON rights_requests;
ALTER TABLE rights_requests DISABLE ROW LEVEL SECURITY;
