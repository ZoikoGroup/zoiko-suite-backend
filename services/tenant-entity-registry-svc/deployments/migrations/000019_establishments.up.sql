-- 000019_establishments.up.sql
--
-- ORG-08 Establishment (ZS-SVC-I-001 s4.8) and the registered office on the address model.
--
-- An establishment is a legal-presence fact with its own id, separate from the address it stands at: an address, by
-- itself, proves no establishment, nexus or registration, and nothing here infers one. Its status moves through a
-- named lifecycle (PROPOSED, ACTIVE, INACTIVE, CLOSED) and every move is an append-only transition, so "what was
-- active on this date" is an exact reconstruction. A material establishment is activated by a principal other than
-- the one who proposed it. Nothing is deleted.

CREATE TABLE establishments (
    establishment_id     UUID PRIMARY KEY,
    tenant_id            UUID NOT NULL REFERENCES tenants(tenant_id),
    legal_entity_id      UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    establishment_type   VARCHAR(32) NOT NULL,
    name                 TEXT NOT NULL,
    address_id           UUID NOT NULL,
    status               VARCHAR(10) NOT NULL DEFAULT 'PROPOSED',
    -- BRANCH and PERMANENT_ESTABLISHMENT can carry legal and tax consequences: independent activation.
    material             BOOLEAN NOT NULL,
    valid_from           DATE NOT NULL,
    valid_to             DATE,
    source_class         VARCHAR(24) NOT NULL,
    evidence_ref         TEXT NOT NULL,
    create_request_id    VARCHAR(128),
    fingerprint          TEXT,
    record_version       BIGINT NOT NULL DEFAULT 1,
    created_by           VARCHAR(255) NOT NULL,
    created_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    activated_by         VARCHAR(255),
    correlation_id       VARCHAR(255),
    CONSTRAINT est_tenant_unique UNIQUE (tenant_id, establishment_id),
    CONSTRAINT est_address_in_tenant FOREIGN KEY (tenant_id, address_id) REFERENCES addresses (tenant_id, address_id),
    CONSTRAINT est_type_known CHECK (establishment_type IN
        ('BRANCH', 'PERMANENT_ESTABLISHMENT', 'PLACE_OF_BUSINESS', 'REPRESENTATIVE_OFFICE', 'OPERATING_SITE', 'EMPLOYMENT_SITE')),
    CONSTRAINT est_status_known CHECK (status IN ('PROPOSED', 'ACTIVE', 'INACTIVE', 'CLOSED')),
    CONSTRAINT est_source_known CHECK (source_class IN
        ('AUTHORITY_REGISTRY', 'AUTHORITY_DOCUMENT', 'TENANT_ASSERTED', 'SYSTEM_IMPORT', 'EXTERNAL_PROVIDER')),
    CONSTRAINT est_present CHECK (btrim(name) <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT est_interval CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT est_closed_has_end CHECK ((status = 'CLOSED') = (valid_to IS NOT NULL)),
    CONSTRAINT est_version_positive CHECK (record_version > 0),
    CONSTRAINT est_request_has_fingerprint CHECK (create_request_id IS NULL OR fingerprint IS NOT NULL),
    -- s4.8: no single operator may create and legally activate a material establishment.
    CONSTRAINT est_independent_activation CHECK (activated_by IS NULL OR NOT material OR activated_by <> created_by)
);
CREATE UNIQUE INDEX est_one_per_request ON establishments (tenant_id, create_request_id) WHERE create_request_id IS NOT NULL;
CREATE INDEX idx_est_entity ON establishments (tenant_id, legal_entity_id, status);

-- An establishment belongs to a legal entity of its own tenant (a plain FK would accept another tenant's entity).
CREATE OR REPLACE FUNCTION establishment_entity_tenant_guard() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM legal_entities WHERE legal_entity_id = NEW.legal_entity_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'an establishment belongs only to a legal entity of its own tenant' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER establishment_entity_tenant_guard BEFORE INSERT OR UPDATE OF legal_entity_id, tenant_id ON establishments
    FOR EACH ROW EXECUTE FUNCTION establishment_entity_tenant_guard();

-- What an establishment was, and when: the as-of read is the latest transition on or before the date.
CREATE TABLE establishment_transitions (
    transition_id    UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    establishment_id UUID NOT NULL,
    seq              INTEGER NOT NULL,
    from_status      VARCHAR(10),
    to_status        VARCHAR(10) NOT NULL,
    effective_date   DATE NOT NULL,
    command          VARCHAR(24) NOT NULL,
    reason           TEXT,
    evidence_ref     TEXT NOT NULL,
    actor            VARCHAR(255) NOT NULL,
    version_before   BIGINT,
    version_after    BIGINT NOT NULL,
    occurred_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id   VARCHAR(255),
    CONSTRAINT et_est_in_tenant FOREIGN KEY (tenant_id, establishment_id) REFERENCES establishments (tenant_id, establishment_id),
    CONSTRAINT et_seq_unique UNIQUE (establishment_id, seq),
    CONSTRAINT et_status_known CHECK (to_status IN ('PROPOSED', 'ACTIVE', 'INACTIVE', 'CLOSED')
        AND (from_status IS NULL OR from_status IN ('PROPOSED', 'ACTIVE', 'INACTIVE', 'CLOSED'))),
    CONSTRAINT et_evidence CHECK (btrim(evidence_ref) <> '')
);
CREATE INDEX idx_et_asof ON establishment_transitions (tenant_id, establishment_id, effective_date, seq);

-- A legal entity uses an address in a stated capacity, for a stated period. The registered office is one.
CREATE TABLE entity_address_links (
    link_id           UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL,
    legal_entity_id   UUID NOT NULL REFERENCES legal_entities(legal_entity_id),
    address_id        UUID NOT NULL,
    link_type         VARCHAR(40) NOT NULL,
    valid_from        DATE NOT NULL,
    valid_to          DATE,
    evidence_ref      TEXT NOT NULL,
    profile_version_id UUID,
    linked_by         VARCHAR(255) NOT NULL,
    linked_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id    VARCHAR(255),
    CONSTRAINT eal_address_in_tenant FOREIGN KEY (tenant_id, address_id) REFERENCES addresses (tenant_id, address_id),
    CONSTRAINT eal_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT eal_evidence CHECK (btrim(evidence_ref) <> '')
);
-- One live registered office per entity.
CREATE UNIQUE INDEX eal_one_live_registered ON entity_address_links (tenant_id, legal_entity_id) WHERE valid_to IS NULL AND link_type = 'REGISTERED';
CREATE INDEX idx_eal_entity ON entity_address_links (tenant_id, legal_entity_id, valid_from);

CREATE OR REPLACE FUNCTION entity_address_link_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'entity address links are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM legal_entities WHERE legal_entity_id = NEW.legal_entity_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'an address links only to a legal entity of its own tenant' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.address_id <> OLD.address_id
           OR NEW.link_type <> OLD.link_type OR NEW.valid_from <> OLD.valid_from OR NEW.evidence_ref <> OLD.evidence_ref
           OR NEW.linked_by <> OLD.linked_by OR NEW.linked_at <> OLD.linked_at THEN
            RAISE EXCEPTION 'an entity address link is immutable; end it and link again' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
            RAISE EXCEPTION 'an ended entity address link cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER entity_address_link_guard BEFORE INSERT OR UPDATE OR DELETE ON entity_address_links
    FOR EACH ROW EXECUTE FUNCTION entity_address_link_guard();

-- Establishment content is immutable; status, version, activation and end move through the commands. Transitions
-- are append-only.
CREATE OR REPLACE FUNCTION establishment_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'establishments are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.establishment_type <> OLD.establishment_type
       OR NEW.name <> OLD.name OR NEW.address_id <> OLD.address_id OR NEW.material <> OLD.material
       OR NEW.valid_from <> OLD.valid_from OR NEW.source_class <> OLD.source_class OR NEW.evidence_ref <> OLD.evidence_ref
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'an establishment is immutable apart from its status' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'CLOSED' THEN
        RAISE EXCEPTION 'a closed establishment cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER establishment_guard BEFORE UPDATE OR DELETE ON establishments
    FOR EACH ROW EXECUTE FUNCTION establishment_guard();

CREATE OR REPLACE FUNCTION establishment_transition_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'establishment transitions are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER establishment_transition_guard BEFORE UPDATE OR DELETE ON establishment_transitions
    FOR EACH ROW EXECUTE FUNCTION establishment_transition_guard();

ALTER TABLE establishments ENABLE ROW LEVEL SECURITY;
ALTER TABLE establishments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON establishments FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE establishment_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE establishment_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON establishment_transitions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE entity_address_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_address_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON entity_address_links FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
