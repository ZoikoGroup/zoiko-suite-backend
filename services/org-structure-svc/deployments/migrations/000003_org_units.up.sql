-- 000003_org_units.up.sql
--
-- ORG-05 Organizational Unit (ZS-SVC-I-001 section 4.5).
--
-- org_units holds identity and lifecycle only. Where a unit sits in the tree,
-- who manages it and which cost/profit center it maps to are separate
-- effective-dated rows, so a past tree can be reconstructed and a reparent
-- never rewrites history. The legacy departments table is untouched.

CREATE TABLE IF NOT EXISTS org_units (
    unit_id         UUID PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    legal_entity_id VARCHAR(255) NOT NULL,
    code            VARCHAR(50)  NOT NULL,
    name            VARCHAR(255) NOT NULL,
    unit_type       VARCHAR(30)  NOT NULL,
    status          VARCHAR(20)  NOT NULL DEFAULT 'DRAFT',
    location_ref    VARCHAR(255) NOT NULL DEFAULT '',
    record_version  BIGINT       NOT NULL DEFAULT 1,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    created_by      VARCHAR(255) NOT NULL DEFAULT '',
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    updated_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    CONSTRAINT org_units_type_valid
        CHECK (unit_type IN ('DEPARTMENT', 'BUSINESS_UNIT', 'FUNCTION', 'DIVISION', 'TEAM')),
    CONSTRAINT org_units_status_valid
        CHECK (status IN ('DRAFT', 'ACTIVE', 'INACTIVE')),
    CONSTRAINT org_units_version_positive CHECK (record_version > 0)
);

-- A code is unique within entity + type while the unit is not retired; an
-- inactive unit keeps its history but frees the code.
CREATE UNIQUE INDEX idx_org_units_code
    ON org_units (tenant_id, legal_entity_id, unit_type, code)
    WHERE status <> 'INACTIVE';
CREATE UNIQUE INDEX idx_org_units_tenant_correlation
    ON org_units (tenant_id, correlation_id)
    WHERE correlation_id <> '';
CREATE INDEX idx_org_units_tenant_entity ON org_units (tenant_id, legal_entity_id);

-- Parent link, effective-dated. valid_to is exclusive; NULL means open-ended.
-- superseded_at is set when a correction replaces a row (recorded-time axis).
CREATE TABLE IF NOT EXISTS org_unit_hierarchy_versions (
    version_id     UUID PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    unit_id        UUID NOT NULL REFERENCES org_units(unit_id),
    parent_unit_id UUID REFERENCES org_units(unit_id),
    valid_from     DATE NOT NULL,
    valid_to       DATE,
    recorded_at    TIMESTAMP WITH TIME ZONE NOT NULL,
    superseded_at  TIMESTAMP WITH TIME ZONE,
    change_reason  TEXT NOT NULL DEFAULT '',
    approval_ref   VARCHAR(255) NOT NULL DEFAULT '',
    evidence_ref   VARCHAR(255) NOT NULL DEFAULT '',
    actor_id       VARCHAR(255) NOT NULL DEFAULT '',
    CONSTRAINT org_hv_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT org_hv_not_self CHECK (parent_unit_id IS NULL OR parent_unit_id <> unit_id)
);

-- At most one open-ended, live version per unit.
CREATE UNIQUE INDEX idx_org_hv_one_open
    ON org_unit_hierarchy_versions (unit_id)
    WHERE valid_to IS NULL AND superseded_at IS NULL;
CREATE INDEX idx_org_hv_unit ON org_unit_hierarchy_versions (tenant_id, unit_id, valid_from);
CREATE INDEX idx_org_hv_parent ON org_unit_hierarchy_versions (tenant_id, parent_unit_id);

-- Manager is a reference to the employee, not a copy of the person.
CREATE TABLE IF NOT EXISTS org_unit_manager_refs (
    ref_id              UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    unit_id             UUID NOT NULL REFERENCES org_units(unit_id),
    manager_employee_id UUID NOT NULL,
    valid_from          DATE NOT NULL,
    valid_to            DATE,
    recorded_at         TIMESTAMP WITH TIME ZONE NOT NULL,
    actor_id            VARCHAR(255) NOT NULL DEFAULT '',
    CONSTRAINT org_mgr_interval CHECK (valid_to IS NULL OR valid_to > valid_from)
);
CREATE UNIQUE INDEX idx_org_mgr_one_open
    ON org_unit_manager_refs (unit_id)
    WHERE valid_to IS NULL;
CREATE INDEX idx_org_mgr_by_manager ON org_unit_manager_refs (tenant_id, manager_employee_id);

-- Cost/profit center mapping. dimension_value_ref stays empty until REF-08
-- (Financial Dimension Registry) exists; ORG-05 does not own the dimension.
CREATE TABLE IF NOT EXISTS org_unit_dimension_mappings (
    mapping_id          UUID PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    unit_id             UUID NOT NULL REFERENCES org_units(unit_id),
    cost_center_code    VARCHAR(50) NOT NULL DEFAULT '',
    profit_center_code  VARCHAR(50) NOT NULL DEFAULT '',
    dimension_value_ref VARCHAR(255) NOT NULL DEFAULT '',
    valid_from          DATE NOT NULL,
    valid_to            DATE,
    recorded_at         TIMESTAMP WITH TIME ZONE NOT NULL,
    actor_id            VARCHAR(255) NOT NULL DEFAULT '',
    CONSTRAINT org_map_interval CHECK (valid_to IS NULL OR valid_to > valid_from)
);
CREATE UNIQUE INDEX idx_org_map_one_open
    ON org_unit_dimension_mappings (unit_id)
    WHERE valid_to IS NULL;

-- Row-level security, same convention as 000001, forced so the table owner
-- is bound by it too.
ALTER TABLE org_units                   ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_unit_hierarchy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_unit_manager_refs       ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_unit_dimension_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_units                   FORCE ROW LEVEL SECURITY;
ALTER TABLE org_unit_hierarchy_versions FORCE ROW LEVEL SECURITY;
ALTER TABLE org_unit_manager_refs       FORCE ROW LEVEL SECURITY;
ALTER TABLE org_unit_dimension_mappings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_org_units ON org_units FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY tenant_isolation_org_unit_hv ON org_unit_hierarchy_versions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY tenant_isolation_org_unit_mgr ON org_unit_manager_refs FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY tenant_isolation_org_unit_map ON org_unit_dimension_mappings FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
