-- Migration: 000007_add_chart_of_accounts.up.sql
--
-- ACC-01 (Chart of Accounts): before this migration, no service owned a
-- validated account master anywhere on this platform — account_code on a
-- journal line was a plain, unvalidated string (see this package's own
-- earlier doc comment). This is the real authority the spec's own
-- Cross-Service Accounting Authority Matrix names: "Posting account
-- master/hierarchy/restrictions," kept a separate authority from journal
-- state per that matrix (ACC-01 "must never own": journal state, posting
-- consequence) even though it's deployed inside this same service — see
-- Document 03 §3.6 and this domain spec's own opening statement: "does
-- not require 18 separately deployed microservices... does require that
-- the authority... remain separately testable and non-bypassable
-- regardless of physical deployment grouping." Separate table, separate
-- handler namespace, separate authz actions — same doctrine.

CREATE TABLE chart_of_accounts (
    account_id                 UUID PRIMARY KEY,
    tenant_id                  UUID NOT NULL,
    account_code               VARCHAR(64) NOT NULL,
    account_name                TEXT NOT NULL,
    account_type                VARCHAR(20) NOT NULL, -- ASSET | LIABILITY | EQUITY | REVENUE | EXPENSE
    parent_account_id           UUID REFERENCES chart_of_accounts(account_id),
    -- Invariant #7: "Control accounts cannot be bypassed by ordinary
    -- manual journals where policy restricts direct posting." An account
    -- is a control account (subledger-controlled, e.g. AP/AR control) AND
    -- separately may have direct posting restricted — two facts, since a
    -- control account with no restriction policy set is a real, allowed
    -- state (a documented decision, not every control account blocks
    -- direct posting by default).
    is_control_account          BOOLEAN NOT NULL DEFAULT FALSE,
    direct_posting_restricted   BOOLEAN NOT NULL DEFAULT FALSE,
    status                      VARCHAR(20) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE | INACTIVE
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id     VARCHAR(255) NOT NULL,

    UNIQUE (tenant_id, account_code)
);

ALTER TABLE chart_of_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE chart_of_accounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON chart_of_accounts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE INDEX idx_coa_tenant_code ON chart_of_accounts (tenant_id, account_code);
CREATE INDEX idx_coa_parent ON chart_of_accounts (parent_account_id);
