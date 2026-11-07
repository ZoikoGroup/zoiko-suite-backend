-- Migration: 000031_account_change_maker_checker.up.sql
--
-- ACC-01 SPEC_DEVIATIONS A4: "SoD: Creator cannot approve material
-- control-account or hierarchy change" was never enforced — ReparentAccount
-- and AmendAccountVersion applied every change, including a reparent or
-- amendment touching a control account, in one call by one principal.
--
-- This is deliberately narrow: an ordinary, non-control account's reparent
-- or amendment is completely unaffected (still one call, applied
-- immediately, exactly as every existing caller and test already depends
-- on). Only a change where a control account is on either side of it —
-- the account itself, or (for a reparent) its new parent — is material
-- enough to require a second, different principal's sign-off, per the
-- spec's own "material control-account or hierarchy change" wording.
--
-- account_change_approvals is a side table, not a new state on
-- account_versions/chart_of_accounts: the existing "at most one open
-- version" invariant (idx_account_versions_current) stays exactly as is,
-- and a pending change simply has not written a version yet. Approval
-- re-runs the same apply logic ReparentAccount/AmendAccountVersion already
-- use for the non-material path, under a fresh row lock, in
-- ApproveAccountChange.
CREATE TABLE account_change_approvals (
    change_id                 UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL,
    account_id                UUID NOT NULL,
    account_code              VARCHAR(64) NOT NULL,
    change_kind               VARCHAR(20) NOT NULL, -- REPARENTED | AMENDED
    expected_version          BIGINT NOT NULL,
    new_parent_account_id     UUID,
    amendment                 JSONB,
    effective_from            TIMESTAMP WITH TIME ZONE NOT NULL,
    reason                    TEXT,
    evidence_refs             TEXT[],
    status                    VARCHAR(16) NOT NULL DEFAULT 'PENDING',
    proposed_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    proposed_by_principal_id  VARCHAR(255) NOT NULL,
    approved_at               TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id  VARCHAR(255)
);

ALTER TABLE account_change_approvals
    ADD CONSTRAINT account_change_approvals_status_check
    CHECK (status IN ('PENDING', 'APPROVED'));

ALTER TABLE account_change_approvals
    ADD CONSTRAINT account_change_approvals_kind_check
    CHECK (change_kind IN ('REPARENTED', 'AMENDED'));

-- Maker-checker, enforced at the row itself (same pattern and same reasoning
-- as financial-close-svc migration 000021): an application-level check runs
-- first, so reaching this constraint means a race or a caller this service
-- does not front — either way the right answer is a named refusal, not a
-- database error surfacing as "store unavailable". mapPgError translates a
-- violation of this constraint to domain.ErrAccountChangeSelfApproval.
ALTER TABLE account_change_approvals
    ADD CONSTRAINT sod_account_change_approver_independent
    CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> proposed_by_principal_id);

-- At most one pending change per account: a second proposal while one is
-- already outstanding would let two different reparents/amendments race
-- against the same expected_version with no way to tell which one a later
-- approval is actually confirming.
CREATE UNIQUE INDEX idx_account_change_approvals_pending
    ON account_change_approvals (tenant_id, account_id)
    WHERE status = 'PENDING';

CREATE INDEX idx_account_change_approvals_tenant_status
    ON account_change_approvals (tenant_id, status);

ALTER TABLE account_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE account_change_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON account_change_approvals
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);
