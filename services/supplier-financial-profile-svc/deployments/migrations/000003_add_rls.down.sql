DROP POLICY IF EXISTS tenant_isolation_policy ON profile_change_events;
ALTER TABLE profile_change_events DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON high_risk_change_requests;
ALTER TABLE high_risk_change_requests DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON payment_terms_periods;
ALTER TABLE payment_terms_periods DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON supplier_financial_profiles;
ALTER TABLE supplier_financial_profiles DISABLE ROW LEVEL SECURITY;
