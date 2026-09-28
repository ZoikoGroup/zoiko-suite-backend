-- 0037_canonical_party_master.sql
-- ZoikoSuite Canonical Party Master (ZS-DATA-001 §10 & Invariant D01/D03)
--
-- Establishes the canonical Party Master schema beneath tenant-entity-registry-svc
-- and counterparty-management-svc.
--
-- DOCTRINE (ZS-DATA-001 §1 & Appendix B2):
-- Customers, suppliers, employees, contractors and financial counterparties are ROLES,
-- never duplicated base identities. One real-world organization or person has one
-- Party identity, with contextual PartyRole records attaching it to legal entities.

CREATE SCHEMA IF NOT EXISTS party_master;

COMMENT ON SCHEMA party_master IS
    'Canonical Party Master. Stable tenant-scoped party identities, contextual roles, verified external identifiers and addresses (ZS-DATA-001 §10).';

GRANT USAGE ON SCHEMA party_master TO zoiko_backend, authenticated;

-- ── parties (ORG-PARTY) ───────────────────────────────────────────────────────

CREATE TABLE party_master.parties (
    party_id                            UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    party_kind                          VARCHAR(20)   NOT NULL, -- 'ORGANIZATION' | 'PERSON'
    display_name                        VARCHAR(255)  NOT NULL,
    legal_name                          VARCHAR(255),
    country_of_registration_or_residence VARCHAR(2), -- ISO 3166-1 alpha-2
    merge_status                        VARCHAR(20)   NOT NULL DEFAULT 'ACTIVE', -- 'ACTIVE' | 'MERGED'
    merged_into_party_id                UUID,
    sensitivity_class                   VARCHAR(20)   NOT NULL DEFAULT 'CONFIDENTIAL', -- 'PUBLIC' | 'INTERNAL' | 'CONFIDENTIAL' | 'RESTRICTED'

    created_by                          VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_party_kind CHECK (party_kind IN ('ORGANIZATION', 'PERSON')),
    CONSTRAINT chk_party_merge_status CHECK (merge_status IN ('ACTIVE', 'MERGED')),
    CONSTRAINT chk_party_sensitivity CHECK (sensitivity_class IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED')),
    CONSTRAINT chk_party_merged_target CHECK (
        (merge_status = 'MERGED' AND merged_into_party_id IS NOT NULL) OR
        (merge_status = 'ACTIVE' AND merged_into_party_id IS NULL)
    ),
    CONSTRAINT fk_party_merge_target FOREIGN KEY (merged_into_party_id) REFERENCES party_master.parties(party_id)
);

CREATE INDEX idx_parties_tenant_kind ON party_master.parties (tenant_id, party_kind);
CREATE INDEX idx_parties_tenant_display ON party_master.parties (tenant_id, display_name);
CREATE INDEX idx_parties_tenant_merge ON party_master.parties (tenant_id, merge_status);

ALTER TABLE party_master.parties ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_master.parties FORCE ROW LEVEL SECURITY;

CREATE POLICY parties_tenant_isolation ON party_master.parties
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON party_master.parties TO zoiko_backend, authenticated;

-- ── party_roles (ORG-ROLE) ───────────────────────────────────────────────────

CREATE TABLE party_master.party_roles (
    party_role_id                       UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    party_id                            UUID          NOT NULL,
    role_type                           VARCHAR(30)   NOT NULL, -- 'CUSTOMER' | 'SUPPLIER' | 'EMPLOYEE' | 'CONTRACTOR' | 'BANK' | etc.
    legal_entity_id                     UUID,         -- Optional legal entity scope; NULL = tenant-wide
    status                              VARCHAR(20)   NOT NULL DEFAULT 'ACTIVE', -- 'ACTIVE' | 'SUSPENDED' | 'CLOSED'

    valid_from                          DATE          NOT NULL,
    valid_to                            DATE,
    recorded_at                         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    superseded_at                       TIMESTAMPTZ,

    CONSTRAINT chk_party_role_type CHECK (role_type IN (
        'CUSTOMER', 'SUPPLIER', 'EMPLOYEE', 'CONTRACTOR',
        'BANK', 'ADVISER', 'TAX_AUTHORITY', 'REGULATOR', 'SHAREHOLDER'
    )),
    CONSTRAINT chk_party_role_status CHECK (status IN ('ACTIVE', 'SUSPENDED', 'CLOSED')),
    CONSTRAINT chk_party_role_valid_dates CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT chk_party_role_superseded CHECK (superseded_at IS NULL OR superseded_at >= recorded_at),
    CONSTRAINT fk_party_roles_party FOREIGN KEY (party_id) REFERENCES party_master.parties(party_id) ON DELETE RESTRICT
);

CREATE INDEX idx_party_roles_tenant_party ON party_master.party_roles (tenant_id, party_id);
CREATE INDEX idx_party_roles_tenant_role ON party_master.party_roles (tenant_id, role_type, status);
CREATE INDEX idx_party_roles_tenant_entity ON party_master.party_roles (tenant_id, legal_entity_id);

ALTER TABLE party_master.party_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_master.party_roles FORCE ROW LEVEL SECURITY;

CREATE POLICY party_roles_tenant_isolation ON party_master.party_roles
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON party_master.party_roles TO zoiko_backend, authenticated;

-- ── party_identifiers (ORG-ID) ───────────────────────────────────────────────

CREATE TABLE party_master.party_identifiers (
    party_identifier_id                 UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    party_id                            UUID          NOT NULL,
    scheme_code                         VARCHAR(30)   NOT NULL, -- 'LEI' | 'COMPANY_REG' | 'VAT' | 'GSTIN' | 'EIN' | etc.
    issuer_or_jurisdiction_id           VARCHAR(255),
    identifier_value                    TEXT          NOT NULL, -- Vault token or encrypted payload
    masked_value                        VARCHAR(255), -- Presentation safe
    valid_from                          DATE,
    valid_to                            DATE,
    verification_status                 VARCHAR(20)   NOT NULL DEFAULT 'UNVERIFIED', -- 'UNVERIFIED' | 'VERIFIED' | 'EXPIRED' | 'REJECTED'
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_identifier_status CHECK (verification_status IN ('UNVERIFIED', 'VERIFIED', 'EXPIRED', 'REJECTED')),
    CONSTRAINT chk_identifier_dates CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to >= valid_from),
    CONSTRAINT fk_party_identifiers_party FOREIGN KEY (party_id) REFERENCES party_master.parties(party_id) ON DELETE RESTRICT
);

CREATE INDEX idx_party_identifiers_tenant_party ON party_master.party_identifiers (tenant_id, party_id);
CREATE INDEX idx_party_identifiers_scheme ON party_master.party_identifiers (tenant_id, scheme_code);

ALTER TABLE party_master.party_identifiers ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_master.party_identifiers FORCE ROW LEVEL SECURITY;

CREATE POLICY party_identifiers_tenant_isolation ON party_master.party_identifiers
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON party_master.party_identifiers TO zoiko_backend, authenticated;

-- ── addresses & address_assignments (ORG-ADDR) ───────────────────────────────

CREATE TABLE party_master.addresses (
    address_id                          UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    address_type                        VARCHAR(30)   NOT NULL DEFAULT 'PHYSICAL',
    line1                               VARCHAR(255)  NOT NULL,
    line2                               VARCHAR(255),
    city                                VARCHAR(100)  NOT NULL,
    subdivision_code                    VARCHAR(20),  -- State / Province (ISO 3166-2)
    postal_code                         VARCHAR(30)   NOT NULL,
    country_code                        VARCHAR(2)    NOT NULL, -- ISO 3166-1 alpha-2
    created_at                          TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_addresses_tenant ON party_master.addresses (tenant_id, country_code);

ALTER TABLE party_master.addresses ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_master.addresses FORCE ROW LEVEL SECURITY;

CREATE POLICY addresses_tenant_isolation ON party_master.addresses
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON party_master.addresses TO zoiko_backend, authenticated;

CREATE TABLE party_master.address_assignments (
    assignment_id                       UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                           UUID          NOT NULL,
    address_id                          UUID          NOT NULL,
    subject_id                          UUID          NOT NULL, -- PartyID or LegalEntityID
    subject_type                        VARCHAR(30)   NOT NULL, -- 'PARTY' | 'LEGAL_ENTITY' | 'ESTABLISHMENT'
    address_type                        VARCHAR(30)   NOT NULL DEFAULT 'PHYSICAL',
    is_primary                          BOOLEAN       NOT NULL DEFAULT FALSE,

    valid_from                          DATE          NOT NULL,
    valid_to                            DATE,
    recorded_at                         TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    superseded_at                       TIMESTAMPTZ,

    CONSTRAINT chk_address_assignment_dates CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT fk_address_assignments_address FOREIGN KEY (address_id) REFERENCES party_master.addresses(address_id) ON DELETE CASCADE
);

CREATE INDEX idx_address_assignments_tenant_subject ON party_master.address_assignments (tenant_id, subject_id, subject_type);

ALTER TABLE party_master.address_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_master.address_assignments FORCE ROW LEVEL SECURITY;

CREATE POLICY address_assignments_tenant_isolation ON party_master.address_assignments
    FOR ALL TO zoiko_backend, authenticated
    USING (tenant_id::text = app.current_tenant_id())
    WITH CHECK (tenant_id::text = app.current_tenant_id());

GRANT SELECT, INSERT, UPDATE ON party_master.address_assignments TO zoiko_backend, authenticated;
