-- 000018_addresses.up.sql
--
-- ORG-08 Address (ZS-SVC-I-001 s4.8, ZS-SVC-U-001 s13, s14.2).
--
-- An address is a stable identity with append-only versions. What was entered is kept exactly; the comparison form
-- beside it only aids matching. A verification is an observation about one version, not a certificate: postal
-- validity and legal registered-office status are different assertions, so verifying never changes what an address
-- is, and a REGISTERED address can only be recorded from authority evidence. Nothing here is deleted.

CREATE TABLE addresses (
    address_id        UUID PRIMARY KEY,
    tenant_id         UUID NOT NULL REFERENCES tenants(tenant_id),
    address_type      VARCHAR(40) NOT NULL,
    current_version   INTEGER NOT NULL DEFAULT 1,
    create_request_id VARCHAR(128),
    fingerprint       TEXT,
    created_by        VARCHAR(255) NOT NULL,
    created_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id    VARCHAR(255),
    CONSTRAINT address_tenant_unique UNIQUE (tenant_id, address_id),
    CONSTRAINT address_type_known CHECK (address_type ~
        '^(REGISTERED|PRINCIPAL_PLACE|OPERATING|BILLING|REMITTANCE|TAX|EMPLOYMENT_ESTABLISHMENT|X_[A-Z0-9_]{1,30})$'),
    CONSTRAINT address_version_positive CHECK (current_version > 0),
    CONSTRAINT address_request_has_fingerprint CHECK (create_request_id IS NULL OR fingerprint IS NOT NULL)
);
CREATE UNIQUE INDEX address_one_per_request ON addresses (tenant_id, create_request_id) WHERE create_request_id IS NOT NULL;

CREATE TABLE address_versions (
    version_id         UUID PRIMARY KEY,
    tenant_id          UUID NOT NULL,
    address_id         UUID NOT NULL,
    version_no         INTEGER NOT NULL,
    lines              TEXT[] NOT NULL,
    locality           TEXT NOT NULL,
    subdivision        TEXT,
    postal_code        TEXT,
    country            CHAR(2) NOT NULL,
    -- The free-text rendering as supplied, kept apart from the structured fields (s14.2).
    formatted          TEXT,
    -- The comparison form. It never replaces the source fields above.
    normalized         TEXT NOT NULL,
    valid_from         DATE NOT NULL,
    valid_to           DATE,
    verification_state VARCHAR(12) NOT NULL DEFAULT 'UNVERIFIED',
    source_class       VARCHAR(24) NOT NULL,
    evidence_ref       TEXT NOT NULL,
    recorded_by        VARCHAR(255) NOT NULL,
    recorded_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    superseded_at      TIMESTAMP WITH TIME ZONE,
    reason             TEXT,
    correlation_id     VARCHAR(255),
    CONSTRAINT av_address_in_tenant FOREIGN KEY (tenant_id, address_id) REFERENCES addresses (tenant_id, address_id),
    CONSTRAINT av_version_unique UNIQUE (address_id, version_no),
    CONSTRAINT av_state_known CHECK (verification_state IN ('UNVERIFIED', 'VERIFIED', 'SUPERSEDED')),
    CONSTRAINT av_source_known CHECK (source_class IN
        ('AUTHORITY_REGISTRY', 'AUTHORITY_DOCUMENT', 'TENANT_ASSERTED', 'SYSTEM_IMPORT', 'EXTERNAL_PROVIDER')),
    CONSTRAINT av_lines CHECK (cardinality(lines) BETWEEN 1 AND 4),
    CONSTRAINT av_interval CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT av_present CHECK (btrim(locality) <> '' AND btrim(evidence_ref) <> '' AND normalized <> ''),
    CONSTRAINT av_country CHECK (country ~ '^[A-Z]{2}$'),
    -- A version that has been superseded says so, and only a superseded version is closed by an amendment.
    CONSTRAINT av_superseded_consistent CHECK ((verification_state = 'SUPERSEDED') = (superseded_at IS NOT NULL))
);
-- One open-ended version at a time: the current one.
CREATE UNIQUE INDEX av_one_open ON address_versions (tenant_id, address_id) WHERE valid_to IS NULL;
CREATE INDEX idx_av_normalized ON address_versions (tenant_id, normalized text_pattern_ops);
CREATE INDEX idx_av_postal ON address_versions (tenant_id, country, postal_code);

-- What a verifier observed about one version. Append-only: a later verification adds a row.
CREATE TABLE address_verifications (
    verification_id UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL,
    address_id      UUID NOT NULL,
    version_no      INTEGER NOT NULL,
    verifier        VARCHAR(24) NOT NULL,
    result          VARCHAR(16) NOT NULL,
    confidence      NUMERIC(3, 2) NOT NULL,
    findings        TEXT[] NOT NULL DEFAULT '{}',
    normalized_seen TEXT NOT NULL,
    observed_by     VARCHAR(255) NOT NULL,
    observed_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id  VARCHAR(255),
    CONSTRAINT avf_version_exists FOREIGN KEY (address_id, version_no) REFERENCES address_versions (address_id, version_no),
    CONSTRAINT avf_result_known CHECK (result IN ('PASSED', 'LOW_CONFIDENCE', 'FAILED')),
    CONSTRAINT avf_confidence CHECK (confidence BETWEEN 0 AND 1)
);
CREATE INDEX idx_avf_address ON address_verifications (tenant_id, address_id, version_no, observed_at);

-- An address is used by a party in a stated capacity, for a stated period.
CREATE TABLE address_party_links (
    link_id        UUID PRIMARY KEY,
    tenant_id      UUID NOT NULL,
    address_id     UUID NOT NULL,
    party_id       UUID NOT NULL,
    link_type      VARCHAR(40) NOT NULL,
    valid_from     DATE NOT NULL,
    valid_to       DATE,
    evidence_ref   TEXT NOT NULL,
    linked_by      VARCHAR(255) NOT NULL,
    linked_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id VARCHAR(255),
    CONSTRAINT apl_address_in_tenant FOREIGN KEY (tenant_id, address_id) REFERENCES addresses (tenant_id, address_id),
    CONSTRAINT apl_party_in_tenant FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT apl_interval CHECK (valid_to IS NULL OR valid_to >= valid_from),
    CONSTRAINT apl_evidence CHECK (btrim(evidence_ref) <> '')
);
CREATE UNIQUE INDEX apl_one_live ON address_party_links (tenant_id, party_id, address_id, link_type) WHERE valid_to IS NULL;
-- s27: two active primary registered addresses are a conflict, so a party has one live registered office.
CREATE UNIQUE INDEX apl_one_live_registered ON address_party_links (tenant_id, party_id) WHERE valid_to IS NULL AND link_type = 'REGISTERED';
CREATE INDEX idx_apl_party ON address_party_links (tenant_id, party_id, valid_from);
CREATE INDEX idx_apl_address ON address_party_links (tenant_id, address_id);

-- A version's content is immutable; only its state and end move, and only forward.
CREATE OR REPLACE FUNCTION address_version_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'address versions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.address_id <> OLD.address_id OR NEW.version_no <> OLD.version_no
       OR NEW.lines <> OLD.lines OR NEW.locality <> OLD.locality
       OR NEW.subdivision IS DISTINCT FROM OLD.subdivision OR NEW.postal_code IS DISTINCT FROM OLD.postal_code
       OR NEW.country <> OLD.country OR NEW.formatted IS DISTINCT FROM OLD.formatted OR NEW.normalized <> OLD.normalized
       OR NEW.valid_from <> OLD.valid_from OR NEW.source_class <> OLD.source_class
       OR NEW.evidence_ref <> OLD.evidence_ref OR NEW.recorded_by <> OLD.recorded_by OR NEW.recorded_at <> OLD.recorded_at THEN
        RAISE EXCEPTION 'an address version is immutable; amend the address' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.verification_state = 'SUPERSEDED' AND NEW.verification_state <> 'SUPERSEDED' THEN
        RAISE EXCEPTION 'a superseded address version cannot be reopened' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
        RAISE EXCEPTION 'a closed address version cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER address_version_guard BEFORE UPDATE OR DELETE ON address_versions
    FOR EACH ROW EXECUTE FUNCTION address_version_guard();

CREATE OR REPLACE FUNCTION address_verification_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'address verifications are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER address_verification_guard BEFORE UPDATE OR DELETE ON address_verifications
    FOR EACH ROW EXECUTE FUNCTION address_verification_guard();

CREATE OR REPLACE FUNCTION address_link_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'address links are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.tenant_id <> OLD.tenant_id OR NEW.address_id <> OLD.address_id OR NEW.party_id <> OLD.party_id
       OR NEW.link_type <> OLD.link_type OR NEW.valid_from <> OLD.valid_from OR NEW.evidence_ref <> OLD.evidence_ref
       OR NEW.linked_by <> OLD.linked_by OR NEW.linked_at <> OLD.linked_at THEN
        RAISE EXCEPTION 'an address link is immutable; end it and link again' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.valid_to IS NOT NULL AND NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
        RAISE EXCEPTION 'an ended address link cannot change its end' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER address_link_guard BEFORE UPDATE OR DELETE ON address_party_links
    FOR EACH ROW EXECUTE FUNCTION address_link_guard();

ALTER TABLE addresses ENABLE ROW LEVEL SECURITY;
ALTER TABLE addresses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON addresses FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE address_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE address_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON address_versions FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE address_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE address_verifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON address_verifications FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

ALTER TABLE address_party_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE address_party_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON address_party_links FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
