-- sod_rules previously had no tenant scoping at all — every rule applied
-- globally, across every tenant, with no way to make one tenant's SoD
-- policy differ from another's. tenant_id is nullable, mirroring
-- jurisdiction_id's existing NULL = globally-applicable convention on this
-- same table (and policy-svc's nullable tenant_id for global policies):
-- a NULL-tenant rule still applies to everyone, a tenant-scoped rule
-- applies only when the caller identifies its tenant.
ALTER TABLE sod_rules
    ADD COLUMN tenant_id UUID;

CREATE INDEX idx_sod_rules_tenant ON sod_rules (tenant_id) WHERE tenant_id IS NOT NULL;
