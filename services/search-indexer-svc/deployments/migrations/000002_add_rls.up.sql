-- Row-level security for the three tenant-scoped tables.
--
-- NULLIF(current_setting('app.tenant_id', true), '')::uuid, not the bare cast.
--
-- This is the trap this estate has already been bitten by. `current_setting`
-- with missing_ok = true returns NULL when the GUC was never set, and the cast
-- of NULL is NULL, which is correct. But set_config(..., '') — which is what a
-- connection reused from the pool after a transaction that set an empty tenant
-- leaves behind — returns the EMPTY STRING, and '' :: uuid RAISES. The policy
-- then errors instead of matching nothing, so a legitimate query on a recycled
-- connection fails as a 500 rather than returning zero rows. NULLIF turns that
-- empty string back into NULL, which compares as false against every row: the
-- fail-closed behaviour the policy was supposed to have all along.
--
-- The platform-scope escape hatch exists for exactly one caller: the Kafka
-- consumer. It processes events for every tenant in one process and cannot
-- know which tenant a message belongs to until it has parsed it, so it cannot
-- set app.tenant_id before opening the transaction that reads the ledger. It
-- sets the trusted tenant from the event envelope per message and uses the
-- tenant-scoped path for the write; the platform flag is used only for the
-- cross-tenant sweeps (restriction verification, checkpoint accounting) that
-- are inherently estate-wide. Kept as an explicit, auditable session flag
-- rather than a role-level RLS exemption, so "this connection is acting with
-- platform authority right now" is never silent.

CREATE OR REPLACE FUNCTION search_indexer_platform_scope() RETURNS boolean AS $$
    SELECT current_setting('app.platform_scope', true) = 'true';
$$ LANGUAGE sql STABLE;

ALTER TABLE projection_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE projection_ledger FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON projection_ledger
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR search_indexer_platform_scope()
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR search_indexer_platform_scope()
    );

ALTER TABLE restriction_tombstones ENABLE ROW LEVEL SECURITY;
ALTER TABLE restriction_tombstones FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON restriction_tombstones
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR search_indexer_platform_scope()
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR search_indexer_platform_scope()
    );

-- search_evidence is append-only in practice and tenant-scoped absolutely.
-- A tenant's evidence rows name its query digests, result counts and the
-- principals that searched; reading another tenant's is a disclosure of who is
-- looking for what, which §9.2 treats as personal data in its own right.
ALTER TABLE search_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON search_evidence
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR search_indexer_platform_scope()
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR search_indexer_platform_scope()
    );
