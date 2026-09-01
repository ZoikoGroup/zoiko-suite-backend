ALTER TABLE supplier_recovery_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE supplier_recovery_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON supplier_recovery_cases
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE recovery_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE recovery_applications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON recovery_applications
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE recovery_commitments ENABLE ROW LEVEL SECURITY;
ALTER TABLE recovery_commitments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON recovery_commitments
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
