-- Initial schema for Employee Master Service (employee-master-svc)

CREATE TABLE IF NOT EXISTS employees (
    employee_id         UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    legal_entity_id     VARCHAR(255) NOT NULL,
    employee_number     VARCHAR(100) NOT NULL,
    first_name          VARCHAR(100) NOT NULL,
    last_name           VARCHAR(100) NOT NULL,
    email               VARCHAR(255) NOT NULL,
    phone               VARCHAR(50),
    job_title           VARCHAR(150) NOT NULL DEFAULT 'Employee',
    department_id       VARCHAR(255),
    manager_employee_id UUID,
    worker_type         VARCHAR(50) NOT NULL, -- 'FULL_TIME', 'PART_TIME', 'CONTRACTOR'
    status              VARCHAR(50) NOT NULL, -- 'ONBOARDING', 'ACTIVE', 'SUSPENDED', 'TERMINATED'
    hire_date           DATE NOT NULL,
    termination_date    DATE,
    effective_from      TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to        TIMESTAMP WITH TIME ZONE,
    created_at          TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at          TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE employees ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policy
CREATE POLICY tenant_isolation_policy ON employees FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance & Uniqueness Indexes
CREATE UNIQUE INDEX idx_employees_tenant_email ON employees (tenant_id, email);
CREATE UNIQUE INDEX idx_employees_tenant_number ON employees (tenant_id, employee_number);
CREATE INDEX idx_employees_tenant_entity_status ON employees (tenant_id, legal_entity_id, status);
CREATE INDEX idx_employees_tenant_dept ON employees (tenant_id, department_id);
CREATE INDEX idx_employees_tenant_manager ON employees (tenant_id, manager_employee_id);