-- Migration: 000001_initial_schema.up.sql
--
-- Owned records for accounts-payable-svc per docs/architecture/03-microservices.md
-- §10.3: vendor invoice headers. Tenant-isolated via Postgres Row-Level
-- Security, matching general-ledger-svc's pattern (this is financial data).
--
-- RLS here is defense-in-depth, not the sole isolation guarantee: this
-- platform's services connect as a Postgres superuser (DB_USER=postgres),
-- and superusers unconditionally bypass RLS regardless of policy — see
-- general-ledger-svc's internal/store/pg_store.go package doc for the full
-- explanation (found via a real CI failure, not theoretical). Every query
-- in this service's store layer filters explicitly by tenant_id in its own
-- SQL for that reason.
--
-- No vendors table: no Vendor Master service exists yet anywhere in this
-- platform, so vendor_id is a plain, unvalidated string column.

CREATE TABLE vendor_invoices (
    invoice_id                       UUID PRIMARY KEY,
    tenant_id                        UUID NOT NULL,
    legal_entity_id                  UUID NOT NULL,
    vendor_id                        VARCHAR(255) NOT NULL,
    invoice_number                   VARCHAR(255) NOT NULL,
    amount                           NUMERIC(18,2) NOT NULL,
    currency_code                    VARCHAR(3) NOT NULL,
    due_date                         DATE NOT NULL,
    status                           VARCHAR(20) NOT NULL,
    created_by_principal_id          VARCHAR(255) NOT NULL,
    validated_by_principal_id        VARCHAR(255),
    approved_by_principal_id         VARCHAR(255),
    payment_requested_by_principal_id VARCHAR(255),
    correlation_id                   VARCHAR(255) NOT NULL,
    created_at                       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    validated_at                     TIMESTAMP WITH TIME ZONE,
    approved_at                      TIMESTAMP WITH TIME ZONE,
    payment_requested_at             TIMESTAMP WITH TIME ZONE,

    UNIQUE (tenant_id, vendor_id, invoice_number)
);

CREATE INDEX idx_vendor_invoices_tenant ON vendor_invoices (tenant_id);
CREATE INDEX idx_vendor_invoices_entity_vendor ON vendor_invoices (legal_entity_id, vendor_id);
CREATE INDEX idx_vendor_invoices_status ON vendor_invoices (status);

ALTER TABLE vendor_invoices ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON vendor_invoices
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);
