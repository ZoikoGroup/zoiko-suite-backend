-- 000024_dimensions.up.sql
--
-- REF-08 Financial Dimension Registry (ZS-SVC-I-001 s4.18).
--
-- A dimension type (cost centre, project, region) holds values. A value's identity (its id and code) never changes;
-- what it is called and where it sits in the hierarchy are effective-dated versions, separate from the identity, so a
-- historical roll-up is always reconstructable and a loop is never allowed. Its status (Draft, Active, Inactive) is a
-- recorded transition per date: an inactive value stays valid for history and rejects new postings unless an explicit,
-- dated exception was recorded. An operational source object (a project, a location) maps to a value by a versioned
-- mapping. This registry owns none of the source objects and none of the posted journal dimension values. Nothing is
-- deleted.

CREATE TABLE dimension_types (
    type_id       UUID PRIMARY KEY,
    tenant_id     UUID NOT NULL REFERENCES tenants(tenant_id),
    type_code     VARCHAR(40) NOT NULL,
    name          TEXT NOT NULL,
    description   TEXT,
    -- Whether values of this type form a hierarchy at all.
    hierarchical  BOOLEAN NOT NULL,
    -- A hierarchy that feeds reporting or control aggregation: a change is independently reviewed before it takes effect.
    controlled    BOOLEAN NOT NULL,
    -- The operational domain that feeds this type, when one does (PROJECT, LOCATION, PRODUCT, ...).
    source_domain VARCHAR(40),
    created_by    VARCHAR(255) NOT NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id VARCHAR(255),
    CONSTRAINT dt_tenant_unique UNIQUE (tenant_id, type_id),
    CONSTRAINT dt_code_unique UNIQUE (tenant_id, type_code),
    CONSTRAINT dt_code_shape CHECK (type_code ~ '^[A-Z][A-Z0-9_]{1,39}$'),
    CONSTRAINT dt_present CHECK (btrim(name) <> ''),
    CONSTRAINT dt_controlled_needs_hierarchy CHECK (hierarchical OR NOT controlled)
);

CREATE TABLE dimension_values (
    value_id        UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    type_id         UUID NOT NULL,
    -- NULL means tenant-wide: usable by every entity. Otherwise the value is that entity's.
    legal_entity_id UUID REFERENCES legal_entities(legal_entity_id),
    code            VARCHAR(40) NOT NULL,
    status          VARCHAR(10) NOT NULL DEFAULT 'DRAFT',
    current_version INTEGER NOT NULL DEFAULT 1,
    record_version  BIGINT NOT NULL DEFAULT 1,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id  VARCHAR(255),
    CONSTRAINT dv_tenant_unique UNIQUE (tenant_id, value_id),
    CONSTRAINT dv_type_in_tenant FOREIGN KEY (tenant_id, type_id) REFERENCES dimension_types (tenant_id, type_id),
    CONSTRAINT dv_status_known CHECK (status IN ('DRAFT', 'ACTIVE', 'INACTIVE')),
    CONSTRAINT dv_code_shape CHECK (code ~ '^[A-Z0-9][A-Z0-9_.-]{0,39}$'),
    CONSTRAINT dv_version_positive CHECK (record_version > 0 AND current_version > 0)
);
-- A code is stable within its scope: unique per type, and per entity for an entity's own values.
CREATE UNIQUE INDEX dv_code_tenant_wide ON dimension_values (tenant_id, type_id, code) WHERE legal_entity_id IS NULL;
CREATE UNIQUE INDEX dv_code_per_entity ON dimension_values (tenant_id, type_id, legal_entity_id, code) WHERE legal_entity_id IS NOT NULL;
CREATE INDEX idx_dv_type ON dimension_values (tenant_id, type_id, status);

CREATE OR REPLACE FUNCTION dimension_value_entity_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.legal_entity_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM legal_entities WHERE legal_entity_id = NEW.legal_entity_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'a dimension value belongs only to a legal entity of its own tenant' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_value_entity_guard BEFORE INSERT OR UPDATE OF legal_entity_id, tenant_id ON dimension_values
    FOR EACH ROW EXECUTE FUNCTION dimension_value_entity_guard();

-- The hierarchy and the label, effective-dated separately from the value's identity. A change is proposed; for a
-- controlled type it takes effect only when someone other than its proposer approves it.
CREATE TABLE dimension_value_versions (
    version_id      UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    value_id        UUID NOT NULL,
    version_no      INTEGER NOT NULL,
    parent_value_id UUID,
    label           TEXT NOT NULL,
    status          VARCHAR(10) NOT NULL,
    valid_from      DATE NOT NULL,
    valid_to        DATE,
    reason          TEXT,
    evidence_ref    TEXT NOT NULL,
    proposed_by     VARCHAR(255) NOT NULL,
    proposed_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    approved_by     VARCHAR(255),
    approved_at     TIMESTAMP WITH TIME ZONE,
    correlation_id  VARCHAR(255),
    CONSTRAINT dvv_value_in_tenant FOREIGN KEY (tenant_id, value_id) REFERENCES dimension_values (tenant_id, value_id),
    CONSTRAINT dvv_parent_in_tenant FOREIGN KEY (tenant_id, parent_value_id) REFERENCES dimension_values (tenant_id, value_id),
    CONSTRAINT dvv_version_unique UNIQUE (value_id, version_no),
    CONSTRAINT dvv_not_own_parent CHECK (parent_value_id IS DISTINCT FROM value_id),
    CONSTRAINT dvv_status_known CHECK (status IN ('PENDING', 'APPROVED')),
    CONSTRAINT dvv_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT dvv_present CHECK (btrim(label) <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT dvv_approved_consistent CHECK ((status = 'APPROVED') = (approved_by IS NOT NULL AND approved_at IS NOT NULL)),
    CONSTRAINT dvv_pending_open CHECK (status <> 'PENDING' OR valid_to IS NULL)
);
CREATE UNIQUE INDEX dvv_one_open ON dimension_value_versions (tenant_id, value_id) WHERE status = 'APPROVED' AND valid_to IS NULL;
CREATE UNIQUE INDEX dvv_one_pending ON dimension_value_versions (tenant_id, value_id) WHERE status = 'PENDING';
CREATE INDEX idx_dvv_value ON dimension_value_versions (tenant_id, value_id, valid_from);
CREATE INDEX idx_dvv_parent ON dimension_value_versions (tenant_id, parent_value_id);

-- A parent is a value of the same type and of a scope that can be shared: a tenant-wide parent for anyone, or the same
-- entity's own. The label and parent of an approved version never change; an independently reviewed hierarchy is
-- approved by someone other than its proposer.
CREATE OR REPLACE FUNCTION dimension_version_guard() RETURNS trigger AS $$
DECLARE
    v dimension_values%ROWTYPE;
    p dimension_values%ROWTYPE;
    t dimension_types%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dimension hierarchy versions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO v FROM dimension_values WHERE tenant_id = NEW.tenant_id AND value_id = NEW.value_id;
    SELECT * INTO t FROM dimension_types WHERE tenant_id = v.tenant_id AND type_id = v.type_id;
    IF TG_OP = 'INSERT' THEN
        IF NEW.parent_value_id IS NOT NULL THEN
            SELECT * INTO p FROM dimension_values WHERE tenant_id = NEW.tenant_id AND value_id = NEW.parent_value_id;
            IF p.type_id <> v.type_id THEN
                RAISE EXCEPTION 'a dimension value''s parent is of the same dimension type' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            IF p.legal_entity_id IS NOT NULL AND p.legal_entity_id IS DISTINCT FROM v.legal_entity_id THEN
                RAISE EXCEPTION 'a dimension value''s parent is tenant-wide or of the same entity' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            IF NOT t.hierarchical THEN
                RAISE EXCEPTION 'this dimension type has no hierarchy' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
    ELSE
        IF NEW.tenant_id <> OLD.tenant_id OR NEW.value_id <> OLD.value_id OR NEW.version_no <> OLD.version_no
           OR NEW.parent_value_id IS DISTINCT FROM OLD.parent_value_id OR NEW.label <> OLD.label OR NEW.valid_from <> OLD.valid_from
           OR NEW.evidence_ref <> OLD.evidence_ref OR NEW.proposed_by <> OLD.proposed_by OR NEW.proposed_at <> OLD.proposed_at THEN
            RAISE EXCEPTION 'a hierarchy version is immutable; propose the next one' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF OLD.status = 'APPROVED' AND (NEW.status <> 'APPROVED' OR NEW.approved_by IS DISTINCT FROM OLD.approved_by) THEN
            RAISE EXCEPTION 'an approved hierarchy version cannot be reopened' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
            RAISE EXCEPTION 'a closed hierarchy version cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    -- s4.18: a hierarchy change affecting reporting or control aggregation is independently reviewed. The first version is a
    -- new node's placement, not a change to an existing aggregation.
    IF NEW.status = 'APPROVED' AND t.controlled AND NEW.version_no > 1 AND NEW.approved_by = NEW.proposed_by THEN
        RAISE EXCEPTION 'a controlled hierarchy change is approved by someone other than its proposer' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_version_guard BEFORE INSERT OR UPDATE OR DELETE ON dimension_value_versions
    FOR EACH ROW EXECUTE FUNCTION dimension_version_guard();

-- What happened to a value's status, and when. An inactive value may carry an explicit, dated posting exception.
CREATE TABLE dimension_value_transitions (
    transition_id    UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    value_id         UUID NOT NULL,
    seq              INTEGER NOT NULL,
    from_status      VARCHAR(10),
    to_status        VARCHAR(10) NOT NULL,
    effective_date   DATE NOT NULL,
    command          VARCHAR(28) NOT NULL,
    reason           TEXT,
    evidence_ref     TEXT NOT NULL,
    -- A controlled exception: new postings are still accepted until this date, though the value is inactive.
    exception_until  DATE,
    actor            VARCHAR(255) NOT NULL,
    version_before   BIGINT,
    version_after    BIGINT NOT NULL,
    occurred_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id   VARCHAR(255),
    CONSTRAINT dvt_value_in_tenant FOREIGN KEY (tenant_id, value_id) REFERENCES dimension_values (tenant_id, value_id),
    CONSTRAINT dvt_seq_unique UNIQUE (value_id, seq),
    CONSTRAINT dvt_status_known CHECK (to_status IN ('DRAFT', 'ACTIVE', 'INACTIVE') AND (from_status IS NULL OR from_status IN ('DRAFT', 'ACTIVE', 'INACTIVE'))),
    CONSTRAINT dvt_evidence CHECK (btrim(evidence_ref) <> ''),
    CONSTRAINT dvt_exception_only_when_inactive CHECK (exception_until IS NULL OR (to_status = 'INACTIVE' AND exception_until > effective_date
        AND btrim(COALESCE(reason, '')) <> ''))
);
CREATE INDEX idx_dvt_asof ON dimension_value_transitions (tenant_id, value_id, effective_date, seq);

-- An operational source object mapped to a value, by version. A new mapping ends the previous one where it begins.
CREATE TABLE dimension_source_mappings (
    mapping_id       UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    type_id          UUID NOT NULL,
    value_id         UUID NOT NULL,
    source_domain    VARCHAR(40) NOT NULL,
    source_object_id TEXT NOT NULL,
    version_no       INTEGER NOT NULL,
    valid_from       DATE NOT NULL,
    valid_to         DATE,
    evidence_ref     TEXT NOT NULL,
    recorded_by      VARCHAR(255) NOT NULL,
    recorded_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id   VARCHAR(255),
    CONSTRAINT dsm_value_in_tenant FOREIGN KEY (tenant_id, value_id) REFERENCES dimension_values (tenant_id, value_id),
    CONSTRAINT dsm_type_in_tenant FOREIGN KEY (tenant_id, type_id) REFERENCES dimension_types (tenant_id, type_id),
    CONSTRAINT dsm_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT dsm_present CHECK (btrim(source_domain) <> '' AND btrim(source_object_id) <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT dsm_version_unique UNIQUE (tenant_id, type_id, source_domain, source_object_id, version_no)
);
-- A source object maps to one value of a type at a time.
CREATE UNIQUE INDEX dsm_one_open ON dimension_source_mappings (tenant_id, type_id, source_domain, source_object_id) WHERE valid_to IS NULL;
CREATE INDEX idx_dsm_value ON dimension_source_mappings (tenant_id, value_id);

CREATE OR REPLACE FUNCTION dimension_mapping_guard() RETURNS trigger AS $$
DECLARE v dimension_values%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dimension source mappings are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v FROM dimension_values WHERE tenant_id = NEW.tenant_id AND value_id = NEW.value_id;
        IF v.type_id <> NEW.type_id THEN
            RAISE EXCEPTION 'a source mapping names a value of its own dimension type' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.type_id <> OLD.type_id OR NEW.value_id <> OLD.value_id OR NEW.source_domain <> OLD.source_domain
       OR NEW.source_object_id <> OLD.source_object_id OR NEW.version_no <> OLD.version_no OR NEW.valid_from <> OLD.valid_from
       OR NEW.evidence_ref <> OLD.evidence_ref OR NEW.recorded_by <> OLD.recorded_by OR NEW.recorded_at <> OLD.recorded_at THEN
        RAISE EXCEPTION 'a source mapping version is immutable; map again' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
        RAISE EXCEPTION 'a closed source mapping cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_mapping_guard BEFORE INSERT OR UPDATE OR DELETE ON dimension_source_mappings
    FOR EACH ROW EXECUTE FUNCTION dimension_mapping_guard();

CREATE OR REPLACE FUNCTION dimension_value_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dimension values are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.type_id <> OLD.type_id OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
       OR NEW.code <> OLD.code OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a dimension value''s identity and code are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_value_guard BEFORE UPDATE OR DELETE ON dimension_values
    FOR EACH ROW EXECUTE FUNCTION dimension_value_guard();

CREATE OR REPLACE FUNCTION dimension_type_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'a dimension type is immutable and never deleted' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_type_guard BEFORE UPDATE OR DELETE ON dimension_types
    FOR EACH ROW EXECUTE FUNCTION dimension_type_guard();

CREATE OR REPLACE FUNCTION dimension_transition_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dimension value transitions are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER dimension_transition_guard BEFORE UPDATE OR DELETE ON dimension_value_transitions
    FOR EACH ROW EXECUTE FUNCTION dimension_transition_guard();

ALTER TABLE dimension_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_types FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON dimension_types FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE dimension_values ENABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_values FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON dimension_values FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE dimension_value_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_value_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON dimension_value_versions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE dimension_value_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_value_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON dimension_value_transitions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE dimension_source_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE dimension_source_mappings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON dimension_source_mappings FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
