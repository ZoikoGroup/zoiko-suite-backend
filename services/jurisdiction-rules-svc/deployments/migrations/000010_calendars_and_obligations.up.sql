-- 000010_calendars_and_obligations.up.sql
-- ZS-JUR-001 Wave 4 (calendar half): regulatory calendars (s15, s27
-- RegulatoryCalendar) and obligation rules (s15, s16). Both are
-- effective-dated, versioned and IMMUTABLE once published, carry source
-- provenance like every other regulatory rule, and are packaged into signed
-- pack artifacts (never read live at runtime). Additive over 000005-000009.

-- ── calendars ───────────────────────────────────────────────────────────────
CREATE TABLE regulatory_calendars (
    calendar_id             UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    calendar_code           VARCHAR(64)  NOT NULL,
    jurisdiction_id         UUID         NOT NULL REFERENCES jurisdictions(jurisdiction_id),
    authority               TEXT         NOT NULL,
    description             TEXT,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    CONSTRAINT uq_regulatory_calendar_code UNIQUE (calendar_code),
    CONSTRAINT ck_calendar_code_shape CHECK (calendar_code ~ '^[a-z0-9][a-z0-9._-]{1,63}$')
);

-- A calendar's content changes only by publishing a NEW version (s15: an
-- amended holiday calendar changes only the due dates it governs; earlier
-- results keep the version they recorded).
CREATE TABLE regulatory_calendar_versions (
    calendar_version_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    calendar_id             UUID         NOT NULL REFERENCES regulatory_calendars(calendar_id),
    version                 INTEGER      NOT NULL,
    effective_from          DATE         NOT NULL,
    timezone                VARCHAR(64)  NOT NULL,
    -- 0 = Sunday ... 6 = Saturday.
    weekend_days            SMALLINT[]   NOT NULL,
    cutoff_time             TIME,
    status                  VARCHAR(16)  NOT NULL DEFAULT 'DRAFT',
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    published_at            TIMESTAMPTZ,
    published_by            TEXT,
    CONSTRAINT uq_calendar_version UNIQUE (calendar_id, version),
    CONSTRAINT ck_calendar_version_status CHECK (status IN ('DRAFT', 'PUBLISHED')),
    CONSTRAINT ck_calendar_version_positive CHECK (version >= 1),
    CONSTRAINT ck_calendar_weekend_range CHECK (weekend_days <@ ARRAY[0,1,2,3,4,5,6]::SMALLINT[] AND cardinality(weekend_days) <= 6),
    CONSTRAINT ck_calendar_published_consistent CHECK (
        (status = 'DRAFT' AND published_at IS NULL AND published_by IS NULL)
        OR (status = 'PUBLISHED' AND published_at IS NOT NULL AND published_by IS NOT NULL AND published_by <> created_by_principal_id))
);

CREATE TABLE regulatory_holidays (
    calendar_version_id UUID NOT NULL REFERENCES regulatory_calendar_versions(calendar_version_id),
    holiday_date        DATE NOT NULL,
    name                TEXT NOT NULL,
    PRIMARY KEY (calendar_version_id, holiday_date),
    CONSTRAINT ck_holiday_name CHECK (length(btrim(name)) > 0)
);

CREATE TABLE calendar_version_sources (
    calendar_version_id UUID NOT NULL REFERENCES regulatory_calendar_versions(calendar_version_id),
    source_id           UUID NOT NULL REFERENCES regulatory_sources(source_id),
    PRIMARY KEY (calendar_version_id, source_id)
);

-- ── obligation rules ────────────────────────────────────────────────────────
CREATE TABLE obligation_rules (
    obligation_rule_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_id             UUID         NOT NULL REFERENCES jurisdictions(jurisdiction_id),
    regime_id                   UUID         REFERENCES regulatory_regimes(regime_id),
    interpretation_id           UUID         REFERENCES interpretation_records(interpretation_id),
    obligation_code             VARCHAR(64)  NOT NULL,
    rule_version                INTEGER      NOT NULL,
    name                        TEXT         NOT NULL,
    period_basis                VARCHAR(16)  NOT NULL,
    anchor                      VARCHAR(24)  NOT NULL,
    offset_months               INTEGER      NOT NULL DEFAULT 0,
    offset_days                 INTEGER      NOT NULL DEFAULT 0,
    offset_to_month_end         BOOLEAN      NOT NULL DEFAULT FALSE,
    business_day_adjustment     VARCHAR(8)   NOT NULL DEFAULT 'NONE',
    calendar_code               VARCHAR(64),
    effective_from              DATE         NOT NULL,
    effective_to                DATE,
    extension_allowed           BOOLEAN      NOT NULL DEFAULT FALSE,
    max_extension_days          INTEGER      NOT NULL DEFAULT 0,
    extension_requires_evidence BOOLEAN      NOT NULL DEFAULT FALSE,
    cutoff_applies              BOOLEAN      NOT NULL DEFAULT FALSE,
    escalation_owner            TEXT,
    escalation_sla_hours        INTEGER,
    status                      VARCHAR(16)  NOT NULL DEFAULT 'DRAFT',
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id     TEXT         NOT NULL,
    published_at                TIMESTAMPTZ,
    published_by                TEXT,
    CONSTRAINT uq_obligation_rule_version UNIQUE (jurisdiction_id, obligation_code, rule_version),
    CONSTRAINT ck_obligation_code CHECK (obligation_code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    CONSTRAINT ck_obligation_basis CHECK (period_basis IN ('MONTHLY','QUARTERLY','ANNUAL','EVENT_DRIVEN','PAYROLL_CYCLE')),
    CONSTRAINT ck_obligation_anchor CHECK (anchor IN ('PERIOD_END','EVENT_DATE','REGISTRATION_DATE','ANNIVERSARY')),
    CONSTRAINT ck_obligation_offsets CHECK (offset_months BETWEEN 0 AND 60 AND offset_days BETWEEN 0 AND 400),
    CONSTRAINT ck_obligation_adjustment CHECK (business_day_adjustment IN ('NONE','NEXT','PREVIOUS')),
    CONSTRAINT ck_obligation_adjustment_calendar CHECK (business_day_adjustment = 'NONE' OR calendar_code IS NOT NULL),
    CONSTRAINT ck_obligation_cutoff_calendar CHECK (NOT cutoff_applies OR calendar_code IS NOT NULL),
    CONSTRAINT ck_obligation_month_end CHECK (NOT offset_to_month_end OR offset_months >= 1),
    CONSTRAINT ck_obligation_extension CHECK (
        (extension_allowed AND max_extension_days BETWEEN 1 AND 366)
        OR (NOT extension_allowed AND max_extension_days = 0 AND NOT extension_requires_evidence)),
    CONSTRAINT ck_obligation_sla CHECK (escalation_sla_hours IS NULL OR escalation_sla_hours >= 1),
    CONSTRAINT ck_obligation_window CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT ck_obligation_status CHECK (status IN ('DRAFT', 'PUBLISHED')),
    CONSTRAINT ck_obligation_published_consistent CHECK (
        (status = 'DRAFT' AND published_at IS NULL AND published_by IS NULL)
        OR (status = 'PUBLISHED' AND published_at IS NOT NULL AND published_by IS NOT NULL AND published_by <> created_by_principal_id))
);
CREATE INDEX idx_obligation_rules_lookup ON obligation_rules (jurisdiction_id, obligation_code, effective_from);

CREATE TABLE obligation_rule_sources (
    obligation_rule_id UUID NOT NULL REFERENCES obligation_rules(obligation_rule_id),
    source_id          UUID NOT NULL REFERENCES regulatory_sources(source_id),
    PRIMARY KEY (obligation_rule_id, source_id)
);

-- ── pack links ──────────────────────────────────────────────────────────────
CREATE TABLE pack_version_calendars (
    pack_version_id     UUID NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    calendar_version_id UUID NOT NULL REFERENCES regulatory_calendar_versions(calendar_version_id),
    PRIMARY KEY (pack_version_id, calendar_version_id)
);
CREATE TABLE pack_version_obligations (
    pack_version_id    UUID NOT NULL REFERENCES jurisdiction_pack_versions(pack_version_id),
    obligation_rule_id UUID NOT NULL REFERENCES obligation_rules(obligation_rule_id),
    PRIMARY KEY (pack_version_id, obligation_rule_id)
);
CREATE TRIGGER trg_pack_version_calendars_guard BEFORE INSERT ON pack_version_calendars
    FOR EACH ROW EXECUTE FUNCTION jur_pack_children_guard();
CREATE TRIGGER trg_pack_version_obligations_guard BEFORE INSERT ON pack_version_obligations
    FOR EACH ROW EXECUTE FUNCTION jur_pack_children_guard();

-- ── immutability ────────────────────────────────────────────────────────────
-- Content (holidays, sources, the version row, the rule row) is editable only
-- while DRAFT. PUBLISHED is final: an amendment is a new version.
CREATE FUNCTION jur_calendar_version_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'PUBLISHED' THEN
        RAISE EXCEPTION 'a published calendar version is immutable; publish a new version' USING ERRCODE = '23514';
    END IF;
    IF NEW.calendar_version_id <> OLD.calendar_version_id OR NEW.calendar_id <> OLD.calendar_id OR NEW.version <> OLD.version
       OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'calendar version identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'PUBLISHED' AND NOT EXISTS (SELECT 1 FROM calendar_version_sources WHERE calendar_version_id = OLD.calendar_version_id) THEN
        RAISE EXCEPTION 'a calendar version cannot be published without a source' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_calendar_version_guard BEFORE UPDATE ON regulatory_calendar_versions FOR EACH ROW EXECUTE FUNCTION jur_calendar_version_guard();
CREATE TRIGGER trg_calendar_version_no_delete BEFORE DELETE ON regulatory_calendar_versions FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

CREATE FUNCTION jur_calendar_child_guard() RETURNS trigger AS $$
DECLARE
    vid UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN vid := OLD.calendar_version_id; ELSE vid := NEW.calendar_version_id; END IF;
    IF (SELECT status FROM regulatory_calendar_versions WHERE calendar_version_id = vid) <> 'DRAFT' THEN
        RAISE EXCEPTION 'a published calendar version is immutable: % cannot change', TG_TABLE_NAME USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_calendar_holidays_guard BEFORE INSERT OR UPDATE OR DELETE ON regulatory_holidays FOR EACH ROW EXECUTE FUNCTION jur_calendar_child_guard();
CREATE TRIGGER trg_calendar_sources_guard  BEFORE INSERT OR UPDATE OR DELETE ON calendar_version_sources FOR EACH ROW EXECUTE FUNCTION jur_calendar_child_guard();

CREATE FUNCTION jur_obligation_guard() RETURNS trigger AS $$
BEGIN
    IF OLD.status = 'PUBLISHED' THEN
        RAISE EXCEPTION 'a published obligation rule is immutable; publish a new version' USING ERRCODE = '23514';
    END IF;
    IF NEW.obligation_rule_id <> OLD.obligation_rule_id OR NEW.jurisdiction_id <> OLD.jurisdiction_id OR NEW.obligation_code <> OLD.obligation_code
       OR NEW.rule_version <> OLD.rule_version OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'obligation rule identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'PUBLISHED' AND NOT EXISTS (SELECT 1 FROM obligation_rule_sources WHERE obligation_rule_id = OLD.obligation_rule_id) THEN
        RAISE EXCEPTION 'an obligation rule cannot be published without a source' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_obligation_guard BEFORE UPDATE ON obligation_rules FOR EACH ROW EXECUTE FUNCTION jur_obligation_guard();
CREATE TRIGGER trg_obligation_no_delete BEFORE DELETE ON obligation_rules FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

CREATE FUNCTION jur_obligation_child_guard() RETURNS trigger AS $$
DECLARE
    rid UUID;
BEGIN
    IF TG_OP = 'DELETE' THEN rid := OLD.obligation_rule_id; ELSE rid := NEW.obligation_rule_id; END IF;
    IF (SELECT status FROM obligation_rules WHERE obligation_rule_id = rid) <> 'DRAFT' THEN
        RAISE EXCEPTION 'a published obligation rule is immutable: its sources cannot change' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_obligation_sources_guard BEFORE INSERT OR UPDATE OR DELETE ON obligation_rule_sources FOR EACH ROW EXECUTE FUNCTION jur_obligation_child_guard();

-- ── evidence: obligation calculations share the decision ledger ─────────────
ALTER TABLE rule_decision_evidence
    ADD COLUMN decision_kind VARCHAR(16) NOT NULL DEFAULT 'RULE',
    ADD CONSTRAINT ck_decision_kind CHECK (decision_kind IN ('RULE', 'OBLIGATION')),
    ADD CONSTRAINT ck_decision_due_has_basis CHECK (
        outcome <> 'DUE_DATE_CALCULATED' OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL));
