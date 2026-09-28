-- Migration: 000016_add_scope_dimensions_and_authority_limits.up.sql
--
-- Adds hierarchical scope dimensions (book_id, org_unit_id) to principal_role_assignments
-- and delegated_authorities (ZS-IAM-001 §4, §10, §21, Scenario A03).
-- Adds foundational authority_limits table and RLS policies (ZS-IAM-001 §12, §21, §22).

BEGIN;

-- ── 1. Hierarchical scope dimensions on principal_role_assignments ───────────

ALTER TABLE principal_role_assignments ADD COLUMN IF NOT EXISTS book_id UUID;
ALTER TABLE principal_role_assignments ADD COLUMN IF NOT EXISTS org_unit_id UUID;

COMMENT ON COLUMN principal_role_assignments.book_id IS
    'Optional accounting book scope (ZS-IAM-001 §4, Scenario A03). NULL means entity-wide or tenant-wide grant across all books.';

COMMENT ON COLUMN principal_role_assignments.org_unit_id IS
    'Optional organizational unit / department scope (ZS-IAM-001 §4). NULL means entity-wide or tenant-wide grant across all org units.';

CREATE INDEX IF NOT EXISTS idx_assignments_scope_lookup
    ON principal_role_assignments (principal_id, legal_entity_id, book_id, org_unit_id, effective_from, effective_to);

-- ── 2. Hierarchical scope dimensions on delegated_authorities ─────────────────

ALTER TABLE delegated_authorities ADD COLUMN IF NOT EXISTS book_id UUID;
ALTER TABLE delegated_authorities ADD COLUMN IF NOT EXISTS org_unit_id UUID;

COMMENT ON COLUMN delegated_authorities.book_id IS
    'Optional accounting book scope for delegation (ZS-IAM-001 §4, §11). NULL confers authority across all books the delegator holds.';

COMMENT ON COLUMN delegated_authorities.org_unit_id IS
    'Optional organizational unit scope for delegation (ZS-IAM-001 §4, §11). NULL confers authority across all org units the delegator holds.';

CREATE INDEX IF NOT EXISTS idx_delegations_scope_lookup
    ON delegated_authorities (delegate_principal_id, legal_entity_id, book_id, org_unit_id, revocation_status);

-- ── 3. Foundation schema for authority_limits ──────────────────────────────────

CREATE TABLE IF NOT EXISTS authority_limits (
    authority_limit_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL,
    principal_id       TEXT,
    role_id            UUID REFERENCES roles(role_id),
    authority_type     VARCHAR(64) NOT NULL, -- e.g. invoice_approval, payment_release, journal_approval
    legal_entity_id    UUID,
    book_id            UUID,
    org_unit_id        UUID,
    currency           VARCHAR(3) NOT NULL DEFAULT 'GBP',
    lower_limit        NUMERIC(18,4) NOT NULL DEFAULT 0,
    upper_limit        NUMERIC(18,4) NOT NULL,
    effective_from     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    effective_to       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_authority_limits_target CHECK (principal_id IS NOT NULL OR role_id IS NOT NULL)
);

COMMENT ON TABLE authority_limits IS
    'Foundational approval/signing authority limits and scope (ZS-IAM-001 §12, §21, §22).';

CREATE INDEX IF NOT EXISTS idx_authority_limits_lookup
    ON authority_limits (tenant_id, authority_type, principal_id, role_id);

CREATE INDEX IF NOT EXISTS idx_authority_limits_scope
    ON authority_limits (tenant_id, legal_entity_id, book_id, org_unit_id);

ALTER TABLE authority_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE authority_limits FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON authority_limits
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMIT;
