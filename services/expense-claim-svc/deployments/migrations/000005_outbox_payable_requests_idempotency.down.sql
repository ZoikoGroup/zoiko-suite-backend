DROP POLICY IF EXISTS tenant_isolation ON expense_claim_events;
CREATE POLICY tenant_isolation ON expense_claim_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON expense_lines;
CREATE POLICY tenant_isolation ON expense_lines
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP POLICY IF EXISTS tenant_isolation ON expense_claims;
CREATE POLICY tenant_isolation ON expense_claims
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

DROP TABLE IF EXISTS expense_claim_idempotency;
DROP TABLE IF EXISTS payable_requests;
DROP FUNCTION IF EXISTS reject_payable_request_mutation();
DROP TABLE IF EXISTS outbox_events;
