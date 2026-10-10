DROP POLICY IF EXISTS tenant_isolation ON accounting_posting_requests;
CREATE POLICY tenant_isolation ON accounting_posting_requests
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
