DROP POLICY IF EXISTS tenant_isolation_policy ON preference_assertions;
ALTER TABLE preference_assertions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON withdrawal_receipts;
ALTER TABLE withdrawal_receipts DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON consent_receipts;
ALTER TABLE consent_receipts DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON presentation_receipts;
ALTER TABLE presentation_receipts DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON notice_versions;
ALTER TABLE notice_versions DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON notices;
ALTER TABLE notices DISABLE ROW LEVEL SECURITY;
