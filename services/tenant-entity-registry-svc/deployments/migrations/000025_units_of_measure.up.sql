-- 000025_units_of_measure.up.sql
--
-- REF-09 Units of Measure (ZS-SVC-I-001 s4.19).
--
-- A quantity dimension (length, mass, volume, count) has one base unit. Every other unit of the dimension converts to it by
-- an exact rational factor, versioned and effective-dated. Because a unit converts only to its own dimension's base, a
-- dimensionally incompatible conversion cannot even be written down, and a conversion between two units is always via the
-- base, in exact arithmetic: no float touches a quantity. A conversion that is not a plain proportion (an offset, a
-- context-dependent rule) has no place here and needs its own specialised rule. A non-standard conversion is approved by
-- someone other than its proposer. This registry owns no quantities: inventory, billable and measured quantities live in the
-- domains that hold them, which pin the conversion version they applied. Nothing is deleted.

CREATE TABLE quantity_dimensions (
    dimension_id   UUID PRIMARY KEY,
    tenant_id      UUID NOT NULL REFERENCES tenants(tenant_id),
    dimension_code VARCHAR(30) NOT NULL,
    name           TEXT NOT NULL,
    description    TEXT,
    created_by     VARCHAR(255) NOT NULL,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id VARCHAR(255),
    CONSTRAINT qd_tenant_unique UNIQUE (tenant_id, dimension_id),
    CONSTRAINT qd_code_unique UNIQUE (tenant_id, dimension_code),
    CONSTRAINT qd_code_shape CHECK (dimension_code ~ '^[A-Z][A-Z0-9_]{1,29}$'),
    CONSTRAINT qd_present CHECK (btrim(name) <> '')
);

CREATE TABLE units (
    unit_id         UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    dimension_id    UUID NOT NULL,
    unit_code       VARCHAR(20) NOT NULL,
    name            TEXT NOT NULL,
    symbol          VARCHAR(20) NOT NULL,
    -- The one unit of the dimension all others convert to. Its factor is 1 by definition.
    is_base         BOOLEAN NOT NULL,
    status          VARCHAR(8) NOT NULL,
    -- How many decimals a quantity in this unit is kept to when a conversion is rounded to it.
    decimals        SMALLINT NOT NULL,
    source_standard TEXT,
    record_version  BIGINT NOT NULL DEFAULT 1,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    retired_on      DATE,
    retired_by      VARCHAR(255),
    retired_at      TIMESTAMP WITH TIME ZONE,
    retire_reason   TEXT,
    correlation_id  VARCHAR(255),
    CONSTRAINT un_tenant_unique UNIQUE (tenant_id, unit_id),
    CONSTRAINT un_code_unique UNIQUE (tenant_id, unit_code),
    CONSTRAINT un_dimension_in_tenant FOREIGN KEY (tenant_id, dimension_id) REFERENCES quantity_dimensions (tenant_id, dimension_id),
    CONSTRAINT un_code_shape CHECK (unit_code ~ '^[A-Z][A-Z0-9_]{0,19}$'),
    CONSTRAINT un_status_known CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    CONSTRAINT un_decimals CHECK (decimals BETWEEN 0 AND 18),
    CONSTRAINT un_present CHECK (btrim(name) <> '' AND btrim(symbol) <> ''),
    -- A base unit needs no conversion, so it is never a draft.
    CONSTRAINT un_base_not_draft CHECK (NOT is_base OR status <> 'DRAFT'),
    CONSTRAINT un_retired_consistent CHECK ((status = 'RETIRED') = (retired_on IS NOT NULL AND retired_by IS NOT NULL AND retired_at IS NOT NULL
        AND btrim(COALESCE(retire_reason, '')) <> '')),
    CONSTRAINT un_version_positive CHECK (record_version > 0)
);
CREATE UNIQUE INDEX un_one_base ON units (tenant_id, dimension_id) WHERE is_base;
CREATE INDEX idx_un_dimension ON units (tenant_id, dimension_id, status);

-- The factor that takes one of the unit to the dimension's base unit, as an exact rational (always reduced, always positive).
CREATE TABLE conversion_versions (
    conversion_id       UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    unit_id             UUID NOT NULL,
    version_no          INTEGER NOT NULL,
    factor_num          NUMERIC NOT NULL,
    factor_den          NUMERIC NOT NULL,
    -- STANDARD is a defined constant (ISO 80000 and the like); ORGANISATION and MANUAL_OVERRIDE are non-standard.
    source_class        VARCHAR(16) NOT NULL,
    source_ref          TEXT NOT NULL,
    status              VARCHAR(10) NOT NULL,
    valid_from          DATE NOT NULL,
    valid_to            DATE,
    supersedes_version  INTEGER,
    reason              TEXT,
    evidence_ref        TEXT NOT NULL,
    proposed_by         VARCHAR(255) NOT NULL,
    proposed_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    approved_by         VARCHAR(255),
    approved_at         TIMESTAMP WITH TIME ZONE,
    correlation_id      VARCHAR(255),
    CONSTRAINT cv_unit_in_tenant FOREIGN KEY (tenant_id, unit_id) REFERENCES units (tenant_id, unit_id),
    CONSTRAINT cv_version_unique UNIQUE (unit_id, version_no),
    CONSTRAINT cv_factor_exact CHECK (factor_num > 0 AND factor_den > 0 AND factor_num = trunc(factor_num) AND factor_den = trunc(factor_den)),
    CONSTRAINT cv_source_known CHECK (source_class IN ('STANDARD', 'ORGANISATION', 'MANUAL_OVERRIDE')),
    CONSTRAINT cv_status_known CHECK (status IN ('PENDING', 'ACTIVE', 'SUPERSEDED')),
    CONSTRAINT cv_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT cv_present CHECK (btrim(source_ref) <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT cv_approved_consistent CHECK ((status = 'PENDING') = (approved_by IS NULL AND approved_at IS NULL)),
    CONSTRAINT cv_pending_open CHECK (status <> 'PENDING' OR valid_to IS NULL),
    CONSTRAINT cv_override_reason CHECK (source_class <> 'MANUAL_OVERRIDE' OR btrim(COALESCE(reason, '')) <> ''),
    -- s4.19: a manual or non-standard conversion is not self-approved.
    CONSTRAINT cv_independent_when_nonstandard CHECK (source_class = 'STANDARD' OR approved_by IS NULL OR approved_by <> proposed_by)
);
CREATE UNIQUE INDEX cv_one_live ON conversion_versions (tenant_id, unit_id) WHERE status = 'ACTIVE';
CREATE UNIQUE INDEX cv_one_pending ON conversion_versions (tenant_id, unit_id) WHERE status = 'PENDING';
CREATE INDEX idx_cv_unit ON conversion_versions (tenant_id, unit_id, valid_from);

-- A conversion belongs to a non-base unit: the base converts to itself by definition.
CREATE OR REPLACE FUNCTION conversion_version_guard() RETURNS trigger AS $$
DECLARE u units%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'conversion versions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO u FROM units WHERE tenant_id = NEW.tenant_id AND unit_id = NEW.unit_id;
        IF u.is_base THEN
            RAISE EXCEPTION 'a base unit converts to itself; it takes no conversion' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.unit_id <> OLD.unit_id OR NEW.version_no <> OLD.version_no OR NEW.factor_num <> OLD.factor_num
       OR NEW.factor_den <> OLD.factor_den OR NEW.source_class <> OLD.source_class OR NEW.source_ref <> OLD.source_ref
       OR NEW.valid_from <> OLD.valid_from OR NEW.evidence_ref <> OLD.evidence_ref OR NEW.proposed_by <> OLD.proposed_by
       OR NEW.proposed_at <> OLD.proposed_at OR NEW.supersedes_version IS DISTINCT FROM OLD.supersedes_version THEN
        RAISE EXCEPTION 'a conversion version is immutable; supersede it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a superseded conversion version cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'ACTIVE' AND (NEW.status = 'PENDING' OR NEW.approved_by IS DISTINCT FROM OLD.approved_by) THEN
        RAISE EXCEPTION 'an approved conversion cannot be reopened' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
        RAISE EXCEPTION 'a closed conversion version cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER conversion_version_guard BEFORE INSERT OR UPDATE OR DELETE ON conversion_versions
    FOR EACH ROW EXECUTE FUNCTION conversion_version_guard();

CREATE OR REPLACE FUNCTION unit_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'units are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.dimension_id <> OLD.dimension_id OR NEW.unit_code <> OLD.unit_code OR NEW.is_base <> OLD.is_base
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a unit''s identity, code and dimension are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired unit cannot change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER unit_guard BEFORE UPDATE OR DELETE ON units
    FOR EACH ROW EXECUTE FUNCTION unit_guard();

CREATE OR REPLACE FUNCTION quantity_dimension_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'a quantity dimension is immutable and never deleted' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER quantity_dimension_guard BEFORE UPDATE OR DELETE ON quantity_dimensions
    FOR EACH ROW EXECUTE FUNCTION quantity_dimension_guard();

ALTER TABLE quantity_dimensions ENABLE ROW LEVEL SECURITY;
ALTER TABLE quantity_dimensions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON quantity_dimensions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE units ENABLE ROW LEVEL SECURITY;
ALTER TABLE units FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON units FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE conversion_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE conversion_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON conversion_versions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
