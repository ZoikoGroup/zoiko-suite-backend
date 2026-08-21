-- Registers the admin console's demo tenant and legal entity in
-- tenant-entity-registry-svc.
--
-- WHY THIS EXISTS. zoiko-suite-fe's DEMO_IDENTITY names a fixed tenant
-- (11111111-…) and legal entity (22222222-…), and every write in this platform
-- authorizes against that legal entity. Nothing had ever registered either of
-- them: the registry's legal_entities table held two unrelated DORMANT entities in
-- two unrelated tenants, and GET /v1/entities/22222222-… answered "not found".
--
-- That went unnoticed because no service ever asked. Authorization checks whether
-- the caller holds a grant ON the entity; it does not check that the entity is
-- real. accounts-receivable-svc now reconciles the legal entity on a write against
-- the caller's verified tenant (see its internal/entity), which turns "the demo
-- entity does not exist" from an invisible inconsistency into a 403 on every
-- invoice — so the demo state has to become true rather than merely unexamined.
--
-- WHY SQL RATHER THAN THE SERVICE'S OWN API. tenant-entity-registry-svc's
-- POST /v1/entities mints the legal_entity_id itself — CreateEntityRequest has no
-- field for it — so a FIXED demo identity cannot be created through it. Seeding a
-- known-id demo fixture is the one job the API cannot do. Everything else about
-- the demo (roles, bundles, grants) goes through the API in seed-demo-rbac.ps1,
-- and should stay that way.
--
-- Idempotent: re-running changes nothing. Safe on a volume that already has data.
--
--   docker exec -i zoiko-postgres psql -v ON_ERROR_STOP=1 -U postgres \
--     -d tenant_entity_registry < seed-demo-registry.sql
--
-- or run seed-demo-registry.ps1, which does exactly that.

\set demo_tenant '11111111-1111-1111-1111-111111111111'
\set demo_entity '22222222-2222-2222-2222-222222222222'
\set demo_principal '33333333-3333-3333-3333-333333333333'
-- Fixed ids for the supporting rows too, so re-running is a no-op rather than
-- accumulating a new policy per run.
\set demo_region '55555555-5555-5555-5555-555555555555'
\set demo_policy '77777777-7777-7777-7777-777777777777'
-- primary_jurisdiction_id and fiscal_calendar_id are UUID NOT NULL with no foreign
-- key: the jurisdictions live in jurisdiction-rules-svc's own database and no
-- Fiscal Calendar service exists on this platform at all, so neither can be
-- referentially checked from here. Fixed placeholders, named as such.
\set demo_jurisdiction '88888888-8888-8888-8888-888888888888'
\set demo_fiscal_calendar '99999999-9999-9999-9999-999999999999'

BEGIN;

INSERT INTO residency_regions (
    residency_region_id, region_code, region_name, cloud_provider, country_code,
    sovereign_flag, active_flag, created_by_principal_id, updated_by_principal_id
) VALUES (
    :'demo_region', 'demo-eu-west', 'Demo EU West', 'local', 'GB',
    false, true, :'demo_principal', :'demo_principal'
) ON CONFLICT (residency_region_id) DO NOTHING;

-- The tenant is inserted BEFORE its residency policy even though it references
-- one: `tenants` carries no foreign keys at all, so the order is free and this way
-- the policy's own tenant_id FK is satisfiable.
INSERT INTO tenants (
    tenant_id, tenant_code, legal_name, trading_name, status,
    default_currency_code, primary_timezone, primary_locale,
    default_data_residency_policy_id, lifecycle_state,
    created_by_principal_id, updated_by_principal_id
) VALUES (
    :'demo_tenant', 'ZOIKO-DEMO', 'Zoiko Demo Holdings Limited', 'Zoiko Demo', 'ACTIVE',
    'GBP', 'Europe/London', 'en-GB',
    :'demo_policy', 'ACTIVE',
    :'demo_principal', :'demo_principal'
) ON CONFLICT (tenant_id) DO NOTHING;

INSERT INTO data_residency_policies (
    data_residency_policy_id, tenant_id, policy_name, policy_code,
    residency_mode, conflict_resolution_mode, active_flag, residency_region_id,
    created_by_principal_id, updated_by_principal_id
) VALUES (
    :'demo_policy', :'demo_tenant', 'Demo residency policy', 'DEMO-RESIDENCY',
    'SINGLE_REGION', 'STRICTEST_WINS', true, :'demo_region',
    :'demo_principal', :'demo_principal'
) ON CONFLICT (data_residency_policy_id) DO NOTHING;

-- ACTIVE, deliberately. accounts-receivable-svc allows a receivable to be raised
-- only against an ACTIVE entity — DORMANT, SUSPENDED and DISSOLVED are all
-- refused — so a demo entity in any other state would refuse every console write
-- for a second, less obvious reason.
INSERT INTO legal_entities (
    legal_entity_id, tenant_id, entity_code, legal_name, trading_name,
    entity_type, default_currency_code, fiscal_calendar_id, entity_status,
    primary_jurisdiction_id, data_residency_policy_id,
    created_by_principal_id, updated_by_principal_id
) VALUES (
    :'demo_entity', :'demo_tenant', 'ZOIKO-DEMO-UK', 'Zoiko Demo UK Limited', 'Zoiko Demo UK',
    'SUBSIDIARY', 'GBP', :'demo_fiscal_calendar', 'ACTIVE',
    :'demo_jurisdiction', :'demo_policy',
    :'demo_principal', :'demo_principal'
) ON CONFLICT (legal_entity_id) DO NOTHING;

COMMIT;

-- Prove it, rather than trusting a silent ON CONFLICT: this is the exact lookup
-- accounts-receivable-svc performs on every write.
SELECT e.legal_entity_id, e.tenant_id, e.entity_code, e.entity_status
FROM legal_entities e
WHERE e.legal_entity_id = :'demo_entity' AND e.tenant_id = :'demo_tenant';
