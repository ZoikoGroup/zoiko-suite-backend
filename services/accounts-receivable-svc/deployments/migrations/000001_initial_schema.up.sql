-- Migration: 000001_initial_schema.up.sql
-- Owned records for accounts-receivable-svc.
-- Tenant-isolated via Postgres Row-Level Security.

CREATE TABLE customer_invoices (
    invoice_id                       UUID PRIMARY KEY,
    tenant_id                        UUID NOT NULL,
    legal_entity_id                  UUID NOT NULL,
    customer_id                      VARCHAR(255) NOT NULL,
    invoice_number                   VARCHAR(255) NOT NULL,
    amount                           NUMERIC(18,2) NOT NULL,
    currency_code                    VARCHAR(3) NOT NULL,
    due_date                         DATE NOT NULL,
    status                           VARCHAR(20) NOT NULL,
    created_by_principal_id          VARCHAR(255) NOT NULL,
    sent_by_principal_id             VARCHAR(255),
    marked_overdue_by_principal_id   VARCHAR(255),
    payment_received_by_principal_id VARCHAR(255),
    correlation_id                   VARCHAR(255) NOT NULL,
    created_at                       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    sent_at                          TIMESTAMP WITH TIME ZONE,
    marked_overdue_at                TIMESTAMP WITH TIME ZONE,
    payment_received_at              TIMESTAMP WITH TIME ZONE,

    UNIQUE (tenant_id, customer_id, invoice_number)
);

CREATE INDEX idx_customer_invoices_tenant ON customer_invoices (tenant_id);
CREATE INDEX idx_customer_invoices_status ON customer_invoices (status);

ALTER TABLE customer_invoices ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON customer_invoices
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
