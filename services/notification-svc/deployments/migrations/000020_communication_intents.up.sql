-- 000020_communication_intents.up.sql
-- ZS-SVC-Y-001 NCD-01 sections 4.1 to 4.3 and 9.2, Wave 2 slice 3.
--
-- A communication INTENT is why a message exists: its purpose class, the evidence it
-- must leave, the channels it may use, and a typed contract for the variables it may
-- be given. Templates (the wording) are bound to an intent in migration 000021; this
-- migration is the registry itself.
--
--   communication_intents          the stable identity (never derived from a display name)
--   communication_intent_versions  the governed content, versioned, one lifecycle per version
--
-- Lifecycle, as for templates: DRAFT -> REVIEW -> APPROVED -> PUBLISHED, with a
-- maker-checker the database enforces. A published version is immutable; a correction
-- is a new version. A change of purpose class is a new version too, because it changes
-- what may block the message (INV-06).
--
-- EFFECTIVE-DATED, BITEMPORAL. A version is published with an effective_from, never in
-- the past. What was in force at time T, as the platform knew it at time K, is the
-- version with the latest effective_from <= T among those published at or before K and
-- not retired by K. Both times are stored, so a historical message can be reconstructed
-- exactly even after later versions or a retirement (9.2).

CREATE TABLE communication_intents (
    intent_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    intent_key              VARCHAR(120) NOT NULL,
    display_name            TEXT         NOT NULL,
    domain_owner            TEXT         NOT NULL,
    status                  VARCHAR(10)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RETIRED')),
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    retired_at              TIMESTAMPTZ,
    retired_by_principal_id VARCHAR(255),
    CONSTRAINT uq_intent_key UNIQUE (tenant_id, intent_key),
    CONSTRAINT ck_intent_key_shape CHECK (intent_key ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
    CONSTRAINT ck_intent_retired_pair CHECK ((status = 'RETIRED') = (retired_at IS NOT NULL))
);

CREATE TABLE communication_intent_versions (
    version_id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id                UUID         NOT NULL REFERENCES communication_intents (intent_id),
    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    version_number           INTEGER      NOT NULL CHECK (version_number >= 1),

    -- What the message IS. S0 security, T0 transactional, A1 operational, L1 lifecycle,
    -- M1 marketing: the classes the policy engine already judges.
    purpose_class            VARCHAR(2)   NOT NULL CHECK (purpose_class IN ('S0', 'T0', 'A1', 'L1', 'M1')),
    -- The minimum proof the delivery must leave (E0 best effort .. E4 formal acknowledgment package).
    evidence_class           VARCHAR(2)   NOT NULL CHECK (evidence_class IN ('E0', 'E1', 'E2', 'E3', 'E4')),
    allowed_channels         JSONB        NOT NULL,
    -- Default false, and only a marketing or lifecycle purpose may ever set it (4.2).
    marketing_allowed        BOOLEAN      NOT NULL DEFAULT FALSE,
    record_requirement       BOOLEAN      NOT NULL DEFAULT FALSE,
    -- name -> {type, required, sensitivity, max_length}. Validated by the service; the
    -- database checks its shape so a direct write cannot store something else.
    variable_contract        JSONB        NOT NULL DEFAULT '{}'::jsonb,

    status                   VARCHAR(12)  NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'REVIEW', 'APPROVED', 'PUBLISHED')),
    created_by_principal_id  VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    validated_at             TIMESTAMPTZ,
    approved_by_principal_id VARCHAR(255),
    approved_at              TIMESTAMPTZ,
    -- Knowledge time (published_at) and transaction time (effective_from).
    effective_from           TIMESTAMPTZ,
    published_at             TIMESTAMPTZ,
    published_by_principal_id VARCHAR(255),

    CONSTRAINT uq_intent_version UNIQUE (intent_id, version_number),
    CONSTRAINT ck_intent_maker_checker CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id),
    CONSTRAINT ck_intent_approval_pair CHECK ((approved_at IS NULL) = (approved_by_principal_id IS NULL)),
    CONSTRAINT ck_intent_publish_pair CHECK ((published_at IS NULL) = (effective_from IS NULL)),
    CONSTRAINT ck_intent_published_has_approval CHECK (published_at IS NULL OR approved_at IS NOT NULL),
    CONSTRAINT ck_intent_channels_shape CHECK (
        jsonb_typeof(allowed_channels) = 'array' AND jsonb_array_length(allowed_channels) BETWEEN 1 AND 5),
    CONSTRAINT ck_intent_contract_shape CHECK (jsonb_typeof(variable_contract) = 'object'),
    CONSTRAINT ck_intent_marketing_scope CHECK (NOT marketing_allowed OR purpose_class IN ('L1', 'M1'))
);

-- Within one intent, effective dates are strictly increasing among published versions,
-- so "the version in force at T" is never ambiguous.
CREATE UNIQUE INDEX uq_intent_effective_from ON communication_intent_versions (intent_id, effective_from)
    WHERE effective_from IS NOT NULL;
CREATE INDEX idx_intent_versions_resolve ON communication_intent_versions (intent_id, effective_from DESC)
    WHERE published_at IS NOT NULL;

ALTER TABLE communication_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE communication_intents FORCE ROW LEVEL SECURITY;
CREATE POLICY intents_tenant_isolation ON communication_intents FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE communication_intent_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE communication_intent_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY intent_versions_tenant_isolation ON communication_intent_versions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Content is immutable from the moment a version is written; so is the identity of an
-- intent. Nothing is ever deleted.
CREATE OR REPLACE FUNCTION guard_intent_version() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' THEN
            RAISE EXCEPTION 'an intent version is created as DRAFT, not %', NEW.status USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'intent versions are never deleted' USING ERRCODE = '23514';
    END IF;

    IF NEW.intent_id IS DISTINCT FROM OLD.intent_id
       OR NEW.version_number IS DISTINCT FROM OLD.version_number
       OR NEW.purpose_class IS DISTINCT FROM OLD.purpose_class
       OR NEW.evidence_class IS DISTINCT FROM OLD.evidence_class
       OR NEW.allowed_channels IS DISTINCT FROM OLD.allowed_channels
       OR NEW.marketing_allowed IS DISTINCT FROM OLD.marketing_allowed
       OR NEW.record_requirement IS DISTINCT FROM OLD.record_requirement
       OR NEW.variable_contract IS DISTINCT FROM OLD.variable_contract
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'intent version % is immutable once written; correct it with a new version', OLD.version_id USING ERRCODE = '23514';
    END IF;
    IF OLD.approved_at IS NOT NULL AND (NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'intent version % is already approved', OLD.version_id USING ERRCODE = '23514';
    END IF;
    IF OLD.published_at IS NOT NULL AND (NEW.published_at IS DISTINCT FROM OLD.published_at OR NEW.effective_from IS DISTINCT FROM OLD.effective_from) THEN
        RAISE EXCEPTION 'intent version % is already published; its dates cannot change', OLD.version_id USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'DRAFT'    AND NEW.status = 'REVIEW'    AND NEW.validated_at IS NOT NULL) OR
        (OLD.status = 'REVIEW'   AND NEW.status = 'APPROVED'  AND NEW.approved_at IS NOT NULL) OR
        (OLD.status = 'APPROVED' AND NEW.status = 'PUBLISHED' AND NEW.published_at IS NOT NULL AND NEW.effective_from IS NOT NULL)) THEN
        RAISE EXCEPTION 'intent version % cannot move from % to % (or is missing the evidence that move requires)',
            OLD.version_id, OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;

    -- Publication is never backdated and never reorders history: the effective date
    -- must be later than every version already published for this intent.
    IF NEW.status = 'PUBLISHED' AND OLD.status <> 'PUBLISHED' THEN
        IF NEW.effective_from < NEW.published_at THEN
            RAISE EXCEPTION 'an intent version cannot be effective before it is published (no backdating)' USING ERRCODE = '23514';
        END IF;
        IF EXISTS (SELECT 1 FROM communication_intent_versions v
                   WHERE v.intent_id = NEW.intent_id AND v.version_id <> NEW.version_id
                     AND v.effective_from IS NOT NULL AND v.effective_from >= NEW.effective_from) THEN
            RAISE EXCEPTION 'an intent version must be effective after every version already published for the intent' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_guard_intent_version
    BEFORE INSERT OR UPDATE OR DELETE ON communication_intent_versions
    FOR EACH ROW EXECUTE FUNCTION guard_intent_version();

CREATE OR REPLACE FUNCTION guard_intent() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'intents are never deleted' USING ERRCODE = '23514';
    END IF;
    IF NEW.intent_id IS DISTINCT FROM OLD.intent_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.intent_key IS DISTINCT FROM OLD.intent_key
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'an intent identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.status = 'RETIRED' AND NEW.status <> 'RETIRED' THEN
        RAISE EXCEPTION 'a retired intent stays retired' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_guard_intent
    BEFORE UPDATE OR DELETE ON communication_intents
    FOR EACH ROW EXECUTE FUNCTION guard_intent();
