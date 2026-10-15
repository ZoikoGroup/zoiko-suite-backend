DROP TABLE IF EXISTS supplier_profile_idempotency;
DROP TABLE IF EXISTS outbox_events;

DROP TRIGGER IF EXISTS supplier_profile_revisions_append_only ON supplier_profile_revisions;
DROP TABLE IF EXISTS supplier_profile_revisions;

DROP INDEX IF EXISTS uq_supplier_financial_profiles_live;

-- Restore the original (nullable-tenant-is-platform-wide) policies.
DROP POLICY IF EXISTS tenant_isolation_policy ON supplier_financial_profiles;
CREATE POLICY tenant_isolation_policy ON supplier_financial_profiles FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON payment_terms_periods;
CREATE POLICY tenant_isolation_policy ON payment_terms_periods FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON high_risk_change_requests;
CREATE POLICY tenant_isolation_policy ON high_risk_change_requests FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation_policy ON profile_change_events;
CREATE POLICY tenant_isolation_policy ON profile_change_events FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE supplier_financial_profiles
    DROP COLUMN IF EXISTS risk_control_flags,
    DROP COLUMN IF EXISTS ap_account_policy,
    DROP COLUMN IF EXISTS tax_classification_refs,
    DROP COLUMN IF EXISTS procurement_category_refs,
    DROP COLUMN IF EXISTS version;
