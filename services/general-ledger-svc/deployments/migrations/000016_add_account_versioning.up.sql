-- Migration: 000016_add_account_versioning.up.sql
--
-- ACC-01 requires a "versioned posting-account master and hierarchy" with
-- "effective-dated account state" and "historical versions queryable"
-- (AccountMaster / AccountHierarchyVersion in the spec's canonical data
-- model). chart_of_accounts remains the current head row so every existing
-- ACC-02/ACC-04 read and the parent FK keep working unchanged. account_versions
-- is the append-only history: every material change writes a new row and closes
-- the previous interval, rather than overwriting the old facts.
--
-- Existing accounts are backfilled as version 1 effective when they were
-- created. New accounts get the same initial version from CreateAccount.

ALTER TABLE chart_of_accounts
    ADD COLUMN version BIGINT NOT NULL DEFAULT 1;

CREATE TABLE account_versions (
    account_version_id        UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL,
    account_id                UUID NOT NULL,
    account_code              VARCHAR(64) NOT NULL,
    version                   BIGINT NOT NULL,
    account_name              TEXT NOT NULL,
    account_type              VARCHAR(20) NOT NULL,
    parent_account_id         UUID,
    is_control_account        BOOLEAN NOT NULL,
    direct_posting_restricted BOOLEAN NOT NULL,
    status                    VARCHAR(20) NOT NULL,
    effective_from            TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to              TIMESTAMP WITH TIME ZONE, -- NULL = currently effective
    change_kind               VARCHAR(20) NOT NULL,      -- CREATED | AMENDED | ACTIVATED | DEACTIVATED | REPARENTED
    change_reason             TEXT,
    evidence_refs             TEXT[],
    created_at                TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id   VARCHAR(255) NOT NULL,

    UNIQUE (tenant_id, account_id, version)
);

-- At most one open (currently effective) version per account, matching the
-- same "one current version" invariant account_mappings already enforces with
-- a partial unique index rather than trusting application code to avoid a race.
CREATE UNIQUE INDEX idx_account_versions_current
    ON account_versions (tenant_id, account_id)
    WHERE effective_to IS NULL;

CREATE INDEX idx_account_versions_asof
    ON account_versions (tenant_id, account_code, effective_from DESC);

ALTER TABLE account_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE account_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON account_versions
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

INSERT INTO account_versions (
    account_version_id, tenant_id, account_id, account_code, version,
    account_name, account_type, parent_account_id, is_control_account,
    direct_posting_restricted, status, effective_from, effective_to,
    change_kind, change_reason, created_at, created_by_principal_id
)
SELECT gen_random_uuid(), tenant_id, account_id, account_code, version,
       account_name, account_type, parent_account_id, is_control_account,
       direct_posting_restricted, status, created_at, NULL,
       'CREATED', 'backfilled from chart_of_accounts', created_at, created_by_principal_id
FROM chart_of_accounts;
