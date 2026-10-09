-- ZS-SVC-Y-001 NCD-01: Communication Intent, Template & Content Registry (§4).
--
-- Two registries, deliberately separate from BIZ-03's template_definitions /
-- template_versions (migration 000005). Those are DOCUMENT templates — they
-- carry no subject, no channel and no intent, and exist for a different
-- specification. A communication template is bound to an intent, a channel
-- and a locale, and is rendered against the intent's typed variable contract.
--
-- Bitemporal by construction (§9.2): every intent version and template version
-- carries an effective time (effective_from — when it governs) and a knowledge
-- time (activated_at / published_at — when the platform learned it governs).
-- GET /v1/intents/{id}/effective resolves against both, so a historical
-- message can be reconstructed exactly as it was issued.

CREATE TABLE IF NOT EXISTS ncd_communication_intents (
    intent_id                   UUID         NOT NULL,
    version                     INTEGER      NOT NULL CHECK (version >= 1),
    tenant_id                   VARCHAR(255) NOT NULL,
    legal_entity_id             VARCHAR(255) NOT NULL,
    -- A human code for the purpose. Informational only: §4.2 requires the id
    -- to be opaque and "never derived from editable display name".
    intent_code                 VARCHAR(120) NOT NULL,
    display_name                VARCHAR(255) NOT NULL,
    purpose_class               VARCHAR(40)  NOT NULL CHECK (purpose_class IN (
                                    'SECURITY_CRITICAL', 'REGULATED_RIGHTS_AFFECTING',
                                    'TRANSACTIONAL_RELATIONSHIP', 'OPERATIONAL_WORKFLOW',
                                    'SERVICE_INFORMATION', 'MARKETING_PROMOTIONAL')),
    domain_owner                VARCHAR(120) NOT NULL,
    sensitivity                 VARCHAR(2)   NOT NULL CHECK (sensitivity IN ('S0','S1','S2','S3')),
    urgency                     VARCHAR(2)   NOT NULL CHECK (urgency IN ('U0','U1','U2','U3')),
    evidence_class              VARCHAR(2)   NOT NULL CHECK (evidence_class IN ('E0','E1','E2','E3','E4')),
    allowed_channels            TEXT[]       NOT NULL CHECK (cardinality(allowed_channels) >= 1),
    fallback_allowed            BOOLEAN      NOT NULL DEFAULT false,
    marketing_allowed           BOOLEAN      NOT NULL DEFAULT false,
    mandatory                   BOOLEAN      NOT NULL DEFAULT false,
    -- §5.3: a mandatory notice may override a CONVENIENCE preference only when
    -- the applicable policy explicitly says so. Never a privacy/legal block.
    preference_override_allowed BOOLEAN      NOT NULL DEFAULT false,
    quiet_hours_policy          VARCHAR(20)  NOT NULL DEFAULT 'RESPECT'
                                    CHECK (quiet_hours_policy IN ('RESPECT', 'EXEMPT')),
    bulk_allowed                BOOLEAN      NOT NULL DEFAULT false,
    record_requirement          BOOLEAN      NOT NULL DEFAULT false,
    ack_requirement             VARCHAR(30)  NOT NULL DEFAULT 'NONE' CHECK (ack_requirement IN (
                                    'NONE', 'RECEIPT', 'AUTHENTICATED_ACK', 'ACCEPTANCE')),
    -- Typed variable contract (§4.2): [{name, type, required, sensitivity,
    -- source_authority, escape, fallback_text, allowed_values}].
    variable_contract           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- [{slot, drc_record_type, max_sensitivity, secure_link}]
    attachment_contract         JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- Approved URL domains for named URL slots (§4.4 URL manipulation).
    approved_url_domains        TEXT[]       NOT NULL DEFAULT '{}',
    default_expiry_seconds      INTEGER      NOT NULL DEFAULT 604800 CHECK (default_expiry_seconds > 0),
    status                      VARCHAR(20)  NOT NULL DEFAULT 'DRAFT'
                                    CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    created_by_principal_id     VARCHAR(255) NOT NULL,
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    approved_by_principal_id    VARCHAR(255),
    activated_at                TIMESTAMPTZ,
    effective_from              TIMESTAMPTZ,
    retired_at                  TIMESTAMPTZ,
    retired_by_principal_id     VARCHAR(255),
    PRIMARY KEY (intent_id, version),
    -- SoD: the author of an intent version cannot activate it.
    CONSTRAINT ncd_intent_sod CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id),
    CONSTRAINT ncd_intent_active_has_times CHECK (status = 'DRAFT' OR (activated_at IS NOT NULL AND effective_from IS NOT NULL AND approved_by_principal_id IS NOT NULL)),
    -- §4.2 marketing_allowed: "default false for security, regulated, payroll
    -- and similar notices" — for the first two it is not configurable at all.
    CONSTRAINT ncd_intent_no_marketing_in_protected CHECK (
        NOT marketing_allowed OR purpose_class NOT IN ('SECURITY_CRITICAL', 'REGULATED_RIGHTS_AFFECTING')),
    CONSTRAINT ncd_intent_marketing_is_marketing CHECK (
        purpose_class <> 'MARKETING_PROMOTIONAL' OR (marketing_allowed AND NOT mandatory)),
    CONSTRAINT ncd_intent_channels_known CHECK (allowed_channels <@ ARRAY['EMAIL','IN_APP','SMS','PUSH']::TEXT[])
);

CREATE INDEX IF NOT EXISTS idx_ncd_intents_tenant ON ncd_communication_intents (tenant_id, intent_id, version DESC);

-- A published intent version is immutable (§4.2: purpose_class "changes create
-- a new version/approval because permission semantics may change"). DRAFT rows
-- may be activated or discarded; nothing else about a version ever changes.
CREATE OR REPLACE FUNCTION ncd_reject_intent_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ncd_communication_intents rows are never deleted';
    END IF;
    IF NEW.intent_id IS DISTINCT FROM OLD.intent_id
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.purpose_class IS DISTINCT FROM OLD.purpose_class
        OR NEW.sensitivity IS DISTINCT FROM OLD.sensitivity
        OR NEW.urgency IS DISTINCT FROM OLD.urgency
        OR NEW.evidence_class IS DISTINCT FROM OLD.evidence_class
        OR NEW.allowed_channels IS DISTINCT FROM OLD.allowed_channels
        OR NEW.fallback_allowed IS DISTINCT FROM OLD.fallback_allowed
        OR NEW.marketing_allowed IS DISTINCT FROM OLD.marketing_allowed
        OR NEW.mandatory IS DISTINCT FROM OLD.mandatory
        OR NEW.preference_override_allowed IS DISTINCT FROM OLD.preference_override_allowed
        OR NEW.quiet_hours_policy IS DISTINCT FROM OLD.quiet_hours_policy
        OR NEW.bulk_allowed IS DISTINCT FROM OLD.bulk_allowed
        OR NEW.record_requirement IS DISTINCT FROM OLD.record_requirement
        OR NEW.ack_requirement IS DISTINCT FROM OLD.ack_requirement
        OR NEW.variable_contract IS DISTINCT FROM OLD.variable_contract
        OR NEW.attachment_contract IS DISTINCT FROM OLD.attachment_contract
        OR NEW.approved_url_domains IS DISTINCT FROM OLD.approved_url_domains
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'intent version %/% is immutable; create a new version', OLD.intent_id, OLD.version;
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'intent version %/% is retired and cannot change', OLD.intent_id, OLD.version;
    END IF;
    IF OLD.status = 'ACTIVE' AND NEW.status <> 'RETIRED' AND NEW.status <> 'ACTIVE' THEN
        RAISE EXCEPTION 'an ACTIVE intent version can only be retired';
    END IF;
    IF OLD.activated_at IS NOT NULL AND (NEW.activated_at IS DISTINCT FROM OLD.activated_at
        OR NEW.effective_from IS DISTINCT FROM OLD.effective_from
        OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'intent version %/% activation is immutable', OLD.intent_id, OLD.version;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_reject_intent_mutation ON ncd_communication_intents;
CREATE TRIGGER trg_ncd_reject_intent_mutation
    BEFORE UPDATE OR DELETE ON ncd_communication_intents
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_intent_mutation();

CREATE TABLE IF NOT EXISTS ncd_templates (
    template_version_id      UUID         PRIMARY KEY,
    -- Stable across versions of one (intent, channel, locale) variant.
    template_id              UUID         NOT NULL,
    version                  INTEGER      NOT NULL CHECK (version >= 1),
    tenant_id                VARCHAR(255) NOT NULL,
    legal_entity_id          VARCHAR(255) NOT NULL,
    intent_id                UUID         NOT NULL,
    -- The intent version this draft was authored against, for lineage.
    intent_version           INTEGER      NOT NULL,
    channel                  VARCHAR(20)  NOT NULL CHECK (channel IN ('EMAIL','IN_APP','SMS','PUSH')),
    locale                   VARCHAR(20)  NOT NULL,
    -- §4.4: fallback only to an EXPLICITLY compatible approved locale.
    compatible_locales       TEXT[]       NOT NULL DEFAULT '{}',
    subject                  TEXT         NOT NULL,
    body                     TEXT         NOT NULL,
    content_hash             VARCHAR(64)  NOT NULL,
    schema_hash              VARCHAR(64)  NOT NULL,
    status                   VARCHAR(20)  NOT NULL DEFAULT 'DRAFT'
                                 CHECK (status IN ('DRAFT','REVIEW','APPROVED','PUBLISHED','RETIRED')),
    created_by_principal_id  VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    validated_at             TIMESTAMPTZ,
    validation_report        JSONB,
    approved_by_principal_id VARCHAR(255),
    approved_at              TIMESTAMPTZ,
    published_by_principal_id VARCHAR(255),
    published_at             TIMESTAMPTZ,
    effective_from           TIMESTAMPTZ,
    retired_at               TIMESTAMPTZ,
    retired_by_principal_id  VARCHAR(255),
    UNIQUE (template_id, version),
    CONSTRAINT ncd_template_sod CHECK (approved_by_principal_id IS NULL OR approved_by_principal_id <> created_by_principal_id),
    CONSTRAINT ncd_template_published_has_times CHECK (status NOT IN ('PUBLISHED') OR (published_at IS NOT NULL AND effective_from IS NOT NULL)),
    CONSTRAINT ncd_template_approved_has_approver CHECK (status NOT IN ('APPROVED','PUBLISHED') OR approved_by_principal_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_ncd_templates_variant ON ncd_templates (tenant_id, intent_id, channel, locale, status);
CREATE INDEX IF NOT EXISTS idx_ncd_templates_template ON ncd_templates (template_id, version DESC);

-- NP-05 / INV-03: "Published template edited in place — write rejected; new
-- version required." Content is immutable from the moment it is written, not
-- merely once published: an approval must certify exactly the bytes that will
-- be sent, so a DRAFT that changes after REVIEW would void the review.
CREATE OR REPLACE FUNCTION ncd_reject_template_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ncd_templates rows are never deleted';
    END IF;
    IF NEW.template_id IS DISTINCT FROM OLD.template_id
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.intent_id IS DISTINCT FROM OLD.intent_id
        OR NEW.intent_version IS DISTINCT FROM OLD.intent_version
        OR NEW.channel IS DISTINCT FROM OLD.channel
        OR NEW.locale IS DISTINCT FROM OLD.locale
        OR NEW.compatible_locales IS DISTINCT FROM OLD.compatible_locales
        OR NEW.subject IS DISTINCT FROM OLD.subject
        OR NEW.body IS DISTINCT FROM OLD.body
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.schema_hash IS DISTINCT FROM OLD.schema_hash
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
    THEN
        RAISE EXCEPTION 'template version % content is immutable; create a new version', OLD.template_version_id;
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'template version % is retired and cannot change', OLD.template_version_id;
    END IF;
    -- Forward-only lifecycle; REVIEW -> DRAFT is the one permitted step back
    -- (a rejection), and it cannot reach a version that was ever approved.
    IF NOT (
        NEW.status = OLD.status
        OR (OLD.status = 'DRAFT'     AND NEW.status IN ('REVIEW','RETIRED'))
        OR (OLD.status = 'REVIEW'    AND NEW.status IN ('DRAFT','APPROVED','RETIRED'))
        OR (OLD.status = 'APPROVED'  AND NEW.status IN ('PUBLISHED','RETIRED'))
        OR (OLD.status = 'PUBLISHED' AND NEW.status = 'RETIRED')
    ) THEN
        RAISE EXCEPTION 'template version % cannot move from % to %', OLD.template_version_id, OLD.status, NEW.status;
    END IF;
    IF OLD.approved_at IS NOT NULL AND (NEW.approved_at IS DISTINCT FROM OLD.approved_at
        OR NEW.approved_by_principal_id IS DISTINCT FROM OLD.approved_by_principal_id) THEN
        RAISE EXCEPTION 'template version % approval is immutable', OLD.template_version_id;
    END IF;
    IF OLD.published_at IS NOT NULL AND (NEW.published_at IS DISTINCT FROM OLD.published_at
        OR NEW.effective_from IS DISTINCT FROM OLD.effective_from) THEN
        RAISE EXCEPTION 'template version % publication is immutable', OLD.template_version_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_reject_template_mutation ON ncd_templates;
CREATE TRIGGER trg_ncd_reject_template_mutation
    BEFORE UPDATE OR DELETE ON ncd_templates
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_template_mutation();

ALTER TABLE ncd_communication_intents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ncd_communication_intents FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ncd_intents_tenant ON ncd_communication_intents;
CREATE POLICY ncd_intents_tenant ON ncd_communication_intents FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE ncd_templates ENABLE ROW LEVEL SECURITY;
ALTER TABLE ncd_templates FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ncd_templates_tenant ON ncd_templates;
CREATE POLICY ncd_templates_tenant ON ncd_templates FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
