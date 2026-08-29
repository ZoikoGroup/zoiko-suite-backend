ALTER TABLE payment_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_proposals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payment_proposals
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE proposal_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE proposal_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON proposal_items
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE proposal_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE proposal_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON proposal_events
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
