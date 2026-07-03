CREATE TABLE residency_regions (
    residency_region_id UUID PRIMARY KEY,
    region_code VARCHAR(255) NOT NULL UNIQUE,
    region_name VARCHAR(255) NOT NULL,
    cloud_provider VARCHAR(255) NOT NULL,
    country_code VARCHAR(2) NOT NULL,
    sovereign_flag BOOLEAN NOT NULL DEFAULT FALSE,
    active_flag BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

CREATE TABLE tenants (
    tenant_id UUID PRIMARY KEY,
    tenant_code VARCHAR(255) NOT NULL UNIQUE,
    legal_name VARCHAR(255) NOT NULL,
    trading_name VARCHAR(255),
    status VARCHAR(50) NOT NULL,
    default_currency_code VARCHAR(3) NOT NULL,
    primary_timezone VARCHAR(50) NOT NULL,
    primary_locale VARCHAR(10) NOT NULL,
    default_data_residency_policy_id UUID NOT NULL,
    lifecycle_state VARCHAR(50) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON tenants
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

CREATE TABLE data_residency_policies (
    data_residency_policy_id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES tenants(tenant_id),
    policy_name VARCHAR(255) NOT NULL,
    policy_code VARCHAR(255) NOT NULL,
    residency_mode VARCHAR(50) NOT NULL,
    conflict_resolution_mode VARCHAR(50) NOT NULL,
    active_flag BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE data_residency_policies ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON data_residency_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

CREATE TABLE legal_entities (
    legal_entity_id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES tenants(tenant_id),
    entity_code VARCHAR(255) NOT NULL UNIQUE,
    legal_name VARCHAR(255) NOT NULL,
    trading_name VARCHAR(255),
    registration_number VARCHAR(255),
    tax_identity_bundle_id UUID,
    entity_type VARCHAR(50) NOT NULL,
    incorporation_date DATE,
    default_currency_code VARCHAR(3) NOT NULL,
    fiscal_calendar_id UUID NOT NULL,
    parent_legal_entity_id UUID REFERENCES legal_entities(legal_entity_id),
    entity_status VARCHAR(50) NOT NULL,
    primary_jurisdiction_id UUID NOT NULL,
    data_residency_policy_id UUID NOT NULL REFERENCES data_residency_policies(data_residency_policy_id),
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE legal_entities ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON legal_entities
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

CREATE TABLE entity_hierarchies (
    hierarchy_id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL REFERENCES tenants(tenant_id),
    parent_legal_entity_id UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    child_legal_entity_id UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    relationship_type VARCHAR(50) NOT NULL,
    effective_from TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE entity_hierarchies ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON entity_hierarchies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id')::UUID);

CREATE TABLE entity_jurisdiction_assignments (
    assignment_id UUID PRIMARY KEY,
    legal_entity_id UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    jurisdiction_id UUID NOT NULL,
    assignment_type VARCHAR(50) NOT NULL,
    effective_from TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to TIMESTAMP WITH TIME ZONE,
    source_basis VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE entity_jurisdiction_assignments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON entity_jurisdiction_assignments
    FOR ALL USING (
        legal_entity_id IN (
            SELECT legal_entity_id FROM legal_entities WHERE tenant_id = current_setting('app.tenant_id')::UUID
        )
    );

CREATE TABLE tax_identity_bundles (
    tax_identity_bundle_id UUID PRIMARY KEY,
    legal_entity_id UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    jurisdiction_id UUID NOT NULL,
    status VARCHAR(50) NOT NULL,
    effective_from TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id VARCHAR(255) NOT NULL,
    updated_by_principal_id VARCHAR(255) NOT NULL
);

ALTER TABLE tax_identity_bundles ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON tax_identity_bundles
    FOR ALL USING (
        legal_entity_id IN (
            SELECT legal_entity_id FROM legal_entities WHERE tenant_id = current_setting('app.tenant_id')::UUID
        )
    );

ALTER TABLE legal_entities ADD CONSTRAINT fk_legal_entities_tax_bundle
    FOREIGN KEY (tax_identity_bundle_id) REFERENCES tax_identity_bundles(tax_identity_bundle_id);
