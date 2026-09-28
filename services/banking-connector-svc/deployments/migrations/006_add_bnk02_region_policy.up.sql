-- BNK-02 region/residency enforcement. Before this migration, a
-- connection's region (bank_connections.region, migration 003) was
-- captured as evidence but never checked against anything — any region
-- string was accepted at REQUESTED->AUTHORIZING and AUTHORIZING->ACTIVE.
--
-- bank_region_policies is the source of truth for "which regions are
-- allowed for a given legal entity," owned by this service (per product
-- decision) rather than looked up from an external registry. A legal
-- entity with zero rows here has no configured restriction — enforcement
-- is opt-in per legal entity, not retroactively deny-by-default for every
-- legal entity that predates this migration.
CREATE TABLE bank_region_policies (
    policy_id                VARCHAR(64) PRIMARY KEY DEFAULT gen_random_uuid()::text,
    tenant_id                VARCHAR(64) NOT NULL,
    legal_entity_id          VARCHAR(64) NOT NULL,
    region                    TEXT NOT NULL,
    created_by_principal_id   VARCHAR(128) NOT NULL,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_bank_region_policies_tenant_entity_region
    ON bank_region_policies (tenant_id, legal_entity_id, region);

ALTER TABLE bank_region_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_region_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON bank_region_policies
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
