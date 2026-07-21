-- 000001_initial_schema.up.sql: Offboarding & Severance Service

CREATE TABLE IF NOT EXISTS termination_requests (
    termination_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    employee_id VARCHAR(64) NOT NULL,
    termination_type VARCHAR(32) NOT NULL,
    reason_code VARCHAR(64) NOT NULL,
    reason_details TEXT,
    notice_period_days INT NOT NULL DEFAULT 0,
    last_working_day DATE NOT NULL,
    effective_from DATE NOT NULL,
    effective_to DATE,
    status VARCHAR(32) NOT NULL DEFAULT 'INITIATED',
    initiated_by VARCHAR(64) NOT NULL,
    approved_by VARCHAR(64),
    approved_at TIMESTAMPTZ,
    severance_amount NUMERIC(15,2) NOT NULL DEFAULT 0.00,
    currency VARCHAR(3) NOT NULL DEFAULT 'USD',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_term_req_tenant_emp ON termination_requests(tenant_id, employee_id);
CREATE INDEX idx_term_req_legal_entity ON termination_requests(tenant_id, legal_entity_id);

ALTER TABLE termination_requests ENABLE ROW LEVEL SECURITY;

CREATE POLICY termination_requests_tenant_isolation ON termination_requests
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Offboarding Checklists
CREATE TABLE IF NOT EXISTS offboarding_checklists (
    checklist_id VARCHAR(64) PRIMARY KEY,
    tenant_id VARCHAR(64) NOT NULL,
    legal_entity_id VARCHAR(64) NOT NULL,
    employee_id VARCHAR(64) NOT NULL,
    termination_id VARCHAR(64) NOT NULL REFERENCES termination_requests(termination_id),
    status VARCHAR(32) NOT NULL DEFAULT 'OPEN',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_offboard_chk_tenant_emp ON offboarding_checklists(tenant_id, employee_id);

ALTER TABLE offboarding_checklists ENABLE ROW LEVEL SECURITY;

CREATE POLICY offboarding_checklists_tenant_isolation ON offboarding_checklists
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Checklist Items
CREATE TABLE IF NOT EXISTS checklist_items (
    item_id VARCHAR(64) PRIMARY KEY,
    checklist_id VARCHAR(64) NOT NULL REFERENCES offboarding_checklists(checklist_id) ON DELETE CASCADE,
    tenant_id VARCHAR(64) NOT NULL,
    category VARCHAR(64) NOT NULL,
    description TEXT NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
    completed_by VARCHAR(64),
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chk_items_checklist ON checklist_items(checklist_id);

ALTER TABLE checklist_items ENABLE ROW LEVEL SECURITY;

CREATE POLICY checklist_items_tenant_isolation ON checklist_items
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
