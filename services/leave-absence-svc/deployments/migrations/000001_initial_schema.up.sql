-- Initial schema for Leave & Absence Service (leave-absence-svc)

CREATE TABLE IF NOT EXISTS leave_types (
    leave_type_id         UUID PRIMARY KEY,
    tenant_id             VARCHAR(255) NOT NULL,
    legal_entity_id       VARCHAR(255) NOT NULL,
    name                  VARCHAR(255) NOT NULL,
    code                  VARCHAR(50) NOT NULL, -- e.g. 'VACATION', 'SICK_LEAVE', 'MATERNITY', 'UNPAID'
    is_paid               BOOLEAN NOT NULL DEFAULT true,
    accrual_rate_per_year NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    max_balance           NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    status                VARCHAR(50) NOT NULL, -- 'ACTIVE', 'INACTIVE'
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at            TIMESTAMP WITH TIME ZONE NOT NULL
);

CREATE TABLE IF NOT EXISTS leave_balances (
    balance_id      UUID PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    employee_id     UUID NOT NULL,
    leave_type_id   UUID NOT NULL REFERENCES leave_types(leave_type_id),
    allocated_hours NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    used_hours      NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    pending_hours   NUMERIC(10, 2) NOT NULL DEFAULT 0.00,
    updated_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    CONSTRAINT unique_tenant_emp_leave_type UNIQUE (tenant_id, employee_id, leave_type_id)
);

CREATE TABLE IF NOT EXISTS leave_requests (
    request_id     UUID PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    employee_id    UUID NOT NULL,
    leave_type_id  UUID NOT NULL REFERENCES leave_types(leave_type_id),
    start_date     DATE NOT NULL,
    end_date       DATE NOT NULL,
    total_hours    NUMERIC(10, 2) NOT NULL,
    reason         TEXT,
    status         VARCHAR(50) NOT NULL, -- 'SUBMITTED', 'APPROVED', 'REJECTED', 'CANCELLED'
    reviewer_id    VARCHAR(255),
    reviewer_notes TEXT,
    reviewed_at    TIMESTAMP WITH TIME ZONE,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at     TIMESTAMP WITH TIME ZONE NOT NULL
);

-- Enable Row-Level Security
ALTER TABLE leave_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_balances ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_requests ENABLE ROW LEVEL SECURITY;

-- Multi-Tenant Security Policies
CREATE POLICY tenant_isolation_leave_types ON leave_types FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_leave_balances ON leave_balances FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE POLICY tenant_isolation_leave_requests ON leave_requests FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Performance Indexes
CREATE INDEX idx_leave_types_tenant_entity ON leave_types (tenant_id, legal_entity_id);
CREATE INDEX idx_leave_balances_tenant_emp ON leave_balances (tenant_id, employee_id);
CREATE INDEX idx_leave_requests_tenant_emp ON leave_requests (tenant_id, employee_id);
CREATE INDEX idx_leave_requests_tenant_status ON leave_requests (tenant_id, status);