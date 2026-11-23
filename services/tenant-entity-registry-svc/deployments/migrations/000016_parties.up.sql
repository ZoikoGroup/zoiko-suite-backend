-- 000016_parties.up.sql
--
-- ORG-07 Party / Counterparty and MDM-01 identity creation (ZS-SVC-I-001 s4.7, ZS-SVC-U-001 s9-s10, s18).
--
-- A party is a stable master identity. What is known about it is effective-dated, sourced facts; who recognises
-- it is external identifiers; what it is to the tenant is explicit, time-aware roles. Facts are never silently
-- overwritten (s19: a verified fact is replaced only by a governed decision), and nothing here is deleted.

CREATE TABLE parties (
    party_id        UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants(tenant_id),
    party_kind      VARCHAR(24) NOT NULL,
    status          VARCHAR(12) NOT NULL DEFAULT 'CANDIDATE',
    -- A LEGAL_ENTITY party links to the ORG-03 legal entity it is. Legal entities stay owned by ORG-03.
    legal_entity_id UUID,
    master_version  BIGINT NOT NULL DEFAULT 1,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id  VARCHAR(255),
    CONSTRAINT party_tenant_unique UNIQUE (tenant_id, party_id),
    CONSTRAINT party_kind_known CHECK (party_kind IN
        ('LEGAL_ENTITY', 'ORGANIZATION_NON_LEGAL', 'NATURAL_PERSON', 'PUBLIC_AUTHORITY')),
    CONSTRAINT party_status_known CHECK (status IN ('CANDIDATE', 'ACTIVE', 'INACTIVE')),
    CONSTRAINT party_version_positive CHECK (master_version > 0),
    CONSTRAINT party_legal_entity_only_for_legal_entity CHECK (legal_entity_id IS NULL OR party_kind = 'LEGAL_ENTITY'),
    CONSTRAINT party_legal_entity_exists FOREIGN KEY (legal_entity_id) REFERENCES legal_entities (legal_entity_id)
);

-- A party may only stand for a legal entity of its own tenant. A plain foreign key would accept another tenant's
-- entity (FK checks bypass row-level security). The composite key 000014 added for this is not used here, so that
-- 000014 still reverses on its own.
CREATE OR REPLACE FUNCTION party_legal_entity_tenant_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.legal_entity_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM legal_entities WHERE legal_entity_id = NEW.legal_entity_id AND tenant_id = NEW.tenant_id) THEN
        RAISE EXCEPTION 'a party stands only for a legal entity of its own tenant' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER party_legal_entity_tenant_guard BEFORE INSERT OR UPDATE OF legal_entity_id, tenant_id ON parties
    FOR EACH ROW EXECUTE FUNCTION party_legal_entity_tenant_guard();

-- A legal entity is one party.
CREATE UNIQUE INDEX party_one_per_legal_entity ON parties (tenant_id, legal_entity_id) WHERE legal_entity_id IS NOT NULL;

CREATE TABLE party_facts (
    fact_id          UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    party_id         UUID NOT NULL,
    fact_type        VARCHAR(24) NOT NULL,
    value            TEXT NOT NULL,
    -- The comparison form (names) or the value itself; the source value above is never altered.
    value_normalized TEXT NOT NULL,
    tokens           TEXT[] NOT NULL DEFAULT '{}',
    language         VARCHAR(35),
    script           VARCHAR(15),
    valid_from       DATE NOT NULL,
    valid_to         DATE,
    -- Knowledge time: what ZoikoSuite knew when. A fact is superseded, never rewritten.
    recorded_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    superseded_at    TIMESTAMP WITH TIME ZONE,
    status           VARCHAR(12) NOT NULL,
    source_class     VARCHAR(24) NOT NULL,
    evidence_ref     TEXT NOT NULL,
    proposed_by      VARCHAR(255) NOT NULL,
    correlation_id   VARCHAR(255),
    recorded_version BIGINT NOT NULL,
    CONSTRAINT pf_party_in_tenant FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT pf_type_known CHECK (fact_type IN
        ('LEGAL_NAME', 'TRADING_NAME', 'FORMER_NAME', 'LOCAL_SCRIPT_NAME', 'DISPLAY_NAME', 'COUNTRY', 'AUTHORITY_TYPE')),
    CONSTRAINT pf_status_known CHECK (status IN ('PROPOSED', 'VERIFIED', 'SUPERSEDED', 'REJECTED')),
    CONSTRAINT pf_source_known CHECK (source_class IN
        ('AUTHORITY_REGISTRY', 'AUTHORITY_DOCUMENT', 'TENANT_ASSERTED', 'SYSTEM_IMPORT', 'EXTERNAL_PROVIDER')),
    CONSTRAINT pf_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT pf_evidence CHECK (btrim(evidence_ref) <> '' AND btrim(value) <> ''),
    -- s10.2: a local-script name carries its language and script.
    CONSTRAINT pf_local_script_meta CHECK (fact_type <> 'LOCAL_SCRIPT_NAME'
        OR (language IS NOT NULL AND script IS NOT NULL))
);
CREATE INDEX idx_pf_party ON party_facts (tenant_id, party_id, fact_type, valid_from);
CREATE INDEX idx_pf_tokens ON party_facts USING GIN (tokens);
-- One verified value at a time for the single-valued facts, from any one start date.
CREATE UNIQUE INDEX pf_one_verified_start
    ON party_facts (tenant_id, party_id, fact_type, valid_from)
    WHERE status = 'VERIFIED' AND fact_type IN ('LEGAL_NAME', 'COUNTRY', 'AUTHORITY_TYPE');

CREATE TABLE party_identifiers (
    identifier_id       UUID PRIMARY KEY,
    tenant_id           UUID NOT NULL,
    party_id            UUID NOT NULL,
    scheme              VARCHAR(24) NOT NULL,
    scope               VARCHAR(64) NOT NULL DEFAULT '',
    value               TEXT NOT NULL,
    value_normalized    TEXT NOT NULL,
    status              VARCHAR(8) NOT NULL DEFAULT 'ACTIVE',
    verification_status VARCHAR(12) NOT NULL DEFAULT 'UNVERIFIED',
    source_class        VARCHAR(24) NOT NULL,
    evidence_ref        TEXT NOT NULL,
    linked_by           VARCHAR(255) NOT NULL,
    linked_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id      VARCHAR(255),
    CONSTRAINT pi_party_in_tenant FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT pi_scheme_known CHECK (scheme IN
        ('COMPANY_REGISTRATION', 'TAX_ID', 'VAT', 'LEI', 'DUNS', 'NATIONAL_ID', 'SOURCE_SYSTEM')),
    CONSTRAINT pi_status_known CHECK (status IN ('ACTIVE', 'ENDED')),
    CONSTRAINT pi_verification_known CHECK (verification_status IN ('UNVERIFIED', 'VERIFIED', 'FAILED')),
    CONSTRAINT pi_value_present CHECK (btrim(value) <> '' AND value_normalized <> '' AND btrim(evidence_ref) <> ''),
    CONSTRAINT pi_source_system_scope CHECK (scheme <> 'SOURCE_SYSTEM' OR scope <> '')
);
-- An identifier names one active party (s18.1 tier A): this is what makes a safe link safe.
CREATE UNIQUE INDEX pi_one_active_owner
    ON party_identifiers (tenant_id, scheme, scope, value_normalized) WHERE status = 'ACTIVE';
CREATE INDEX idx_pi_party ON party_identifiers (tenant_id, party_id);

CREATE TABLE party_roles (
    role_id        UUID PRIMARY KEY,
    tenant_id      UUID NOT NULL,
    party_id       UUID NOT NULL,
    role_type      VARCHAR(16) NOT NULL,
    valid_from     DATE NOT NULL,
    valid_to       DATE,
    evidence_ref   TEXT NOT NULL,
    added_by       VARCHAR(255) NOT NULL,
    added_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    removed_by     VARCHAR(255),
    removed_at     TIMESTAMP WITH TIME ZONE,
    remove_reason  TEXT,
    correlation_id VARCHAR(255),
    CONSTRAINT pr_party_in_tenant FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT pr_role_known CHECK (role_type IN
        ('CUSTOMER', 'SUPPLIER', 'COUNTERPARTY', 'SIGNATORY', 'BANK', 'REGULATOR', 'LANDLORD', 'EMPLOYER_LINK', 'OTHER_GOVERNED')),
    CONSTRAINT pr_interval CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT pr_evidence CHECK (btrim(evidence_ref) <> ''),
    -- A removal is a decision with an author and a reason; the three travel together.
    CONSTRAINT pr_removal_consistent CHECK (
        (removed_at IS NULL AND removed_by IS NULL AND remove_reason IS NULL)
        OR (removed_at IS NOT NULL AND removed_by IS NOT NULL AND btrim(COALESCE(remove_reason, '')) <> ''))
);
-- One live role of a type at a time; removing it frees the slot and the removed row stays as history.
CREATE UNIQUE INDEX pr_one_live ON party_roles (tenant_id, party_id, role_type) WHERE removed_at IS NULL;
CREATE INDEX idx_pr_role ON party_roles (tenant_id, role_type) WHERE removed_at IS NULL;

-- Stewardship: what a machine will not decide. A create that is ambiguous, an identifier that is claimed twice and
-- a fact that contradicts a verified one each open a case; merge and split (MDM-05) decide them.
CREATE TABLE resolution_cases (
    case_id           UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(tenant_id),
    case_type         VARCHAR(24) NOT NULL,
    status            VARCHAR(12) NOT NULL DEFAULT 'OPEN',
    subject_party_id  UUID,
    -- [{party_id, tier, score, basis[]}]: why the case exists, explainable and tenant-scoped.
    candidates        JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The create request held back, so a steward can create it or link it once decided.
    held_request      JSONB,
    create_request_id VARCHAR(128),
    reason            TEXT NOT NULL,
    opened_by         VARCHAR(255) NOT NULL,
    opened_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id    VARCHAR(255),
    record_version    BIGINT NOT NULL DEFAULT 1,
    CONSTRAINT rc_tenant_unique UNIQUE (tenant_id, case_id),
    CONSTRAINT rc_type_known CHECK (case_type IN ('DUPLICATE_REVIEW', 'IDENTIFIER_CONFLICT', 'FACT_CONFLICT')),
    CONSTRAINT rc_status_known CHECK (status IN ('OPEN', 'RESOLVED', 'DISMISSED')),
    CONSTRAINT rc_subject_in_tenant FOREIGN KEY (tenant_id, subject_party_id) REFERENCES parties (tenant_id, party_id)
);
CREATE INDEX idx_rc_open ON resolution_cases (tenant_id, opened_at) WHERE status = 'OPEN';

-- create_request_id makes a replay return what the first call returned, whatever that was (a party, an existing
-- party or a case), and makes the same id carrying a different request a refusal.
CREATE TABLE party_create_requests (
    tenant_id         UUID NOT NULL REFERENCES tenants(tenant_id),
    create_request_id VARCHAR(128) NOT NULL,
    fingerprint       TEXT NOT NULL,
    outcome           VARCHAR(20) NOT NULL,
    party_id          UUID,
    case_id           UUID,
    match_candidate   JSONB,
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, create_request_id),
    CONSTRAINT pcr_outcome_known CHECK (outcome IN ('CREATED', 'MATCHED_EXISTING', 'REVIEW_REQUIRED')),
    CONSTRAINT pcr_outcome_has_subject CHECK (
        (outcome IN ('CREATED', 'MATCHED_EXISTING') AND party_id IS NOT NULL)
        OR (outcome = 'REVIEW_REQUIRED' AND case_id IS NOT NULL))
);

-- History is the point: a fact moves between statuses and gains superseded_at, but its content, source and author
-- never change and it is never deleted; a request record never changes at all.
CREATE OR REPLACE FUNCTION party_fact_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'party facts are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.party_id <> OLD.party_id OR NEW.fact_type <> OLD.fact_type
       OR NEW.value <> OLD.value OR NEW.value_normalized <> OLD.value_normalized OR NEW.valid_from <> OLD.valid_from
       OR NEW.source_class <> OLD.source_class OR NEW.evidence_ref <> OLD.evidence_ref
       OR NEW.proposed_by <> OLD.proposed_by OR NEW.recorded_at <> OLD.recorded_at THEN
        RAISE EXCEPTION 'a party fact is immutable; supersede it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER party_fact_guard BEFORE UPDATE OR DELETE ON party_facts
    FOR EACH ROW EXECUTE FUNCTION party_fact_guard();

CREATE OR REPLACE FUNCTION party_request_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'party create requests are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER party_request_guard BEFORE UPDATE OR DELETE ON party_create_requests
    FOR EACH ROW EXECUTE FUNCTION party_request_guard();

ALTER TABLE parties ENABLE ROW LEVEL SECURITY;
ALTER TABLE parties FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON parties FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE party_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_facts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON party_facts FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE party_identifiers ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_identifiers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON party_identifiers FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE party_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON party_roles FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE resolution_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE resolution_cases FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON resolution_cases FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE party_create_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_create_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON party_create_requests FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
