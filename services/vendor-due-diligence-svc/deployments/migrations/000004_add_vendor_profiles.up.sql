-- Priority 6 Row 76: Add missing VendorProfile entity per Doc 04 §14.1 / §2056
CREATE TABLE IF NOT EXISTS vendor_profiles (
    vendor_profile_id    UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL,
    legal_entity_id      UUID NOT NULL,
    counterparty_id      VARCHAR(255) NOT NULL,
    vendor_name          VARCHAR(255) NOT NULL,
    vendor_status        VARCHAR(30) NOT NULL DEFAULT 'ONBOARDING',
    due_diligence_status VARCHAR(30) NOT NULL DEFAULT 'PENDING',
    risk_rating          VARCHAR(30) NOT NULL DEFAULT 'UNRATED',
    approved_at          TIMESTAMP WITH TIME ZONE,
    approved_by          VARCHAR(255),
    created_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT uk_vendor_profile_counterparty UNIQUE (tenant_id, legal_entity_id, counterparty_id)
);

ALTER TABLE vendor_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE vendor_profiles FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON vendor_profiles FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE INDEX IF NOT EXISTS idx_vendor_profiles_tenant_entity ON vendor_profiles (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_vendor_profiles_counterparty   ON vendor_profiles (tenant_id, counterparty_id);
CREATE INDEX IF NOT EXISTS idx_vendor_profiles_status         ON vendor_profiles (vendor_status);

