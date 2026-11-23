-- Migration: 000029_account_mapping_governance.up.sql
--
-- ACC-02 (Account Mapping) full governance lifecycle. Migration 000008 gave
-- account_mappings an effective-dated history but no approval state at all:
-- SetAccountMapping could put a mapping straight into "currently effective"
-- in one call, including mappings onto cash/tax/control accounts, with no
-- maker-checker. This closes that gap with the spec's own state model:
-- Draft -> Approved -> Active -> Superseded.
--
-- Each row IS a version (CreateMapping inserts a new DRAFT row rather than
-- mutating an existing one), so no separate version table is needed here —
-- unlike chart_of_accounts/account_versions, which project a mutable head
-- row over an immutable history table.

-- status defaults to ACTIVE, not DRAFT: a row inserted with no lifecycle
-- metadata at all (every pre-migration row, and every direct INSERT a test
-- fixture or operational script makes without going through CreateMapping)
-- is exactly what SetAccountMapping always produced before this migration —
-- immediately resolvable. Only CreateMapping explicitly writes DRAFT; the
-- default here is for rows nothing in this migration's lifecycle wrote.
ALTER TABLE account_mappings
    ADD COLUMN status VARCHAR(16) NOT NULL DEFAULT 'ACTIVE',
    ADD COLUMN is_high_risk BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN approved_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN approved_by_principal_id VARCHAR(255),
    ADD COLUMN activated_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN superseded_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN superseded_by_mapping_id UUID;

ALTER TABLE account_mappings
    ADD CONSTRAINT account_mappings_status_check
    CHECK (status IN ('DRAFT', 'APPROVED', 'ACTIVE', 'SUPERSEDED'));

-- Backfill: every row 000008's SetAccountMapping ever wrote was put straight
-- into effect with no approval step. Rather than pretend an approval
-- happened that didn't, these are backfilled as self-approved by their own
-- creator (approved_by = created_by) — an honest record of how they were
-- actually created, not a fabricated maker-checker trail.
UPDATE account_mappings m
SET status = CASE WHEN m.effective_to IS NULL THEN 'ACTIVE' ELSE 'SUPERSEDED' END,
    is_high_risk = COALESCE(
        (SELECT is_control_account FROM chart_of_accounts c
         WHERE c.tenant_id = m.tenant_id AND c.account_code = m.account_code), FALSE),
    approved_at = m.effective_from,
    approved_by_principal_id = m.created_by_principal_id,
    activated_at = m.effective_from,
    superseded_at = CASE WHEN m.effective_to IS NOT NULL THEN m.effective_to ELSE NULL END;

-- The replacement for idx_account_mappings_current (migration 000008): at
-- most one ACTIVE mapping per (tenant, mapping_key) is the structural
-- guarantee behind ACC-02's "zero or multiple equally applicable mappings
-- block automated posting; no fallback guessing" — enforced here so it can
-- never happen, not merely checked for afterwards.
DROP INDEX IF EXISTS idx_account_mappings_current;
CREATE UNIQUE INDEX idx_account_mappings_active
    ON account_mappings (tenant_id, mapping_key)
    WHERE status = 'ACTIVE';

CREATE INDEX idx_account_mappings_superseded_by ON account_mappings (superseded_by_mapping_id)
    WHERE superseded_by_mapping_id IS NOT NULL;
