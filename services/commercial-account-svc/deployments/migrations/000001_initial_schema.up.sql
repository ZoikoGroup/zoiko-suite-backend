-- 000001_initial_schema.up.sql
-- commercial-account-svc — Plane 1 (Zoiko Commercial Account) per
-- docs/original_doc/zoiko_suite_doc7.txt §3, §A4, §A3.
--
-- organization_id is this platform's existing tenant_id (tenant-entity-
-- registry-svc's tenants.tenant_id) — a plain string reference, not a
-- cross-database FK, same posture this platform already uses for
-- legal_entity_id references between services.

CREATE TABLE commercial_accounts (
    commercial_account_id   UUID PRIMARY KEY,
    organization_id         UUID NOT NULL,
    legal_customer_name     VARCHAR(255) NOT NULL,
    billing_currency_code   VARCHAR(3) NOT NULL,
    contact_email           VARCHAR(255),
    contract_reference      VARCHAR(255),
    processor_customer_ref  VARCHAR(255),
    status                  VARCHAR(50) NOT NULL,
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL
);

CREATE INDEX idx_commercial_accounts_organization ON commercial_accounts (organization_id);

-- One commercial account per organization — doc7 §A4: the commercial
-- account is the verified customer record; an organization does not have
-- two competing billing identities.
CREATE UNIQUE INDEX idx_commercial_accounts_org_unique ON commercial_accounts (organization_id);

-- Memberships answer "does this principal belong to this organization at
-- all" (doc7 §A3) — deliberately separate from authorization-svc's RBAC
-- tables (principal_role_assignments etc.), which answer "what may they do".
-- Deactivate, never delete — doc7 §A6: historical attribution must survive
-- a member's removal.
CREATE TABLE memberships (
    membership_id            UUID PRIMARY KEY,
    principal_id             VARCHAR(255) NOT NULL,
    organization_id          UUID NOT NULL,
    workspace_id             UUID,
    legal_entity_id          UUID,
    status                   VARCHAR(50) NOT NULL,
    effective_from           TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to             TIMESTAMP WITH TIME ZONE,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE INDEX idx_memberships_organization ON memberships (organization_id);
CREATE INDEX idx_memberships_principal ON memberships (principal_id);

-- A principal has at most one ACTIVE membership per (organization, workspace,
-- legal_entity) scope — COALESCE handles the nullable narrower-scope columns
-- the same way policy-svc/tenant-entity-registry-svc handle their own
-- nullable scoping columns.
CREATE UNIQUE INDEX idx_memberships_active_scope_unique ON memberships (
    principal_id,
    organization_id,
    COALESCE(workspace_id, '00000000-0000-0000-0000-000000000000'::UUID),
    COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000'::UUID)
) WHERE status = 'ACTIVE';
