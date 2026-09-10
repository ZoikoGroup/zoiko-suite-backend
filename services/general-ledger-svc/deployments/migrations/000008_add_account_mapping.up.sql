-- Migration: 000008_add_account_mapping.up.sql
--
-- ACC-02 (Account Mapping): "owns effective-dated account mapping. Must
-- never own: Ledger entry, source business fact." This maps an opaque,
-- caller-declared business concept (mapping_key — its MEANING belongs to
-- whichever domain service declares it, e.g. "EXPENSE_CATEGORY:MEALS";
-- this service never interprets it) to a real, chart-registered account
-- code, versioned over time — never a destructive overwrite, same
-- "effective-dated, never mutated in place" doctrine as this platform's
-- other reference registries (e.g. privacy-purpose-registry-svc).

CREATE TABLE account_mappings (
    account_mapping_id       UUID PRIMARY KEY,
    tenant_id                UUID NOT NULL,
    mapping_key               VARCHAR(255) NOT NULL,
    account_code              VARCHAR(64) NOT NULL,
    effective_from             TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to               TIMESTAMP WITH TIME ZONE, -- NULL = currently effective
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id    VARCHAR(255) NOT NULL
);

-- At most one CURRENT (effective_to IS NULL) mapping per (tenant,
-- mapping_key) — the same "one current version" invariant this platform
-- already enforces elsewhere via a partial unique index rather than
-- trusting application code alone to never race two concurrent writers.
CREATE UNIQUE INDEX idx_account_mappings_current
    ON account_mappings (tenant_id, mapping_key)
    WHERE effective_to IS NULL;

ALTER TABLE account_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE account_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON account_mappings
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

CREATE INDEX idx_account_mappings_tenant_key ON account_mappings (tenant_id, mapping_key, effective_from DESC);
