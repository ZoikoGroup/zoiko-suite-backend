-- 000024: legal entity standing, projected from tenant-entity-registry-svc.
--
-- entity.status.changed was consumed only to invalidate the cache, so a
-- SUSPENDED or DISSOLVED entity stayed one anybody holding a grant could act
-- in (Doc 03 §8.3 consumes entity.scope.updated; ZS-IAM-001 §7 stage 6 makes
-- negative controls override grants). /v1/authorize now denies in a SUSPENDED
-- or DISSOLVED entity and permits a DORMANT one with an obligation.
--
-- NO ROW MEANS NO RESTRICTION, like principal_status_projection: the registry
-- is authoritative and this table changes no outcome until it has spoken.
CREATE TABLE IF NOT EXISTS entity_status_projection (
    legal_entity_id    UUID         PRIMARY KEY,
    tenant_id          UUID         NOT NULL,
    -- The registry's EntityStatus verbatim (DRAFT, VERIFIED, ACTIVE, DORMANT,
    -- SUSPENDED, DISSOLVED). Data only.
    status             VARCHAR(32)  NOT NULL,
    -- The event's effective_at: orders out-of-order redeliveries.
    status_changed_at  TIMESTAMPTZ  NOT NULL,
    projected_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

ALTER TABLE entity_status_projection ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_status_projection FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON entity_status_projection;
CREATE POLICY tenant_isolation_policy ON entity_status_projection
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR current_setting('app.platform_scope', true) = 'true'
    )
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_authorization') THEN
        GRANT SELECT, INSERT, UPDATE ON entity_status_projection TO app_authorization;
    END IF;
END
$$;
