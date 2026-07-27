-- Initial schema for Org Structure Service (org-structure-svc)

CREATE TABLE IF NOT EXISTS departments (
    department_id        UUID PRIMARY KEY,
    tenant_id            VARCHAR(255) NOT NULL,
    legal_entity_id      VARCHAR(255) NOT NULL,
    name                 VARCHAR(255) NOT NULL,
    code                 VARCHAR(50) NOT NULL,
    cost_center_code     VARCHAR(50) NOT NULL,
    parent_department_id UUID REFERENCES departments(department_id),
    status               VARCHAR(50) NOT NULL DEFAULT 'ACTIVE',
    created_at           TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at           TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS positions (
    position_id       UUID PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    legal_entity_id   VARCHAR(255) NOT NULL,
    department_id     UUID NOT NULL REFERENCES departments(department_id),
    title             VARCHAR(255) NOT NULL,
    code              VARCHAR(50) NOT NULL,
    job_level         VARCHAR(50) NOT NULL,
    max_headcount     INT NOT NULL DEFAULT 1,
    current_headcount INT NOT NULL DEFAULT 0,
    status            VARCHAR(50) NOT NULL DEFAULT 'ACTIVE',
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at        TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS org_assignments (
    assignment_id       UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    employee_id         UUID NOT NULL,
    department_id       UUID NOT NULL REFERENCES departments(department_id),
    position_id         UUID NOT NULL REFERENCES positions(position_id),
    manager_employee_id UUID,
    effective_from      DATE NOT NULL,
    effective_to        DATE,
    status              VARCHAR(50) NOT NULL DEFAULT 'ACTIVE',
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE departments ENABLE ROW LEVEL SECURITY;
ALTER TABLE positions ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_assignments ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_departments ON departments FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_positions ON positions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_org_assignments ON org_assignments FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_departments_tenant_entity ON departments (tenant_id, legal_entity_id);
CREATE INDEX idx_positions_tenant_dept ON positions (tenant_id, department_id);
CREATE INDEX idx_org_assignments_tenant_emp ON org_assignments (tenant_id, employee_id);
CREATE INDEX idx_org_assignments_tenant_mgr ON org_assignments (tenant_id, manager_employee_id);