-- 000028_party_contact_points.up.sql
--
-- MDM-01 governed contact points (ZS-SVC-U-001): an email address a same-tenant party can be reached at, proposed by one
-- principal and verified on evidence by another. The address is stored in clear because delivery needs it; it is protected by
-- tenant row-level security and the PARTY_CONTACT_READ permission, and it is never placed in an event (events carry the
-- sha256 digest). Cross-tenant contact lookup is out of scope (MDM-I-05, MDM-I-29). Nothing is deleted; RETIRED is final.

CREATE TABLE party_contact_points (
    contact_point_id UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL REFERENCES tenants(tenant_id),
    party_id         UUID NOT NULL,
    channel          VARCHAR(16) NOT NULL,
    value            TEXT NOT NULL,
    value_digest     CHAR(64) NOT NULL,
    purpose          VARCHAR(16) NOT NULL DEFAULT 'GENERAL',
    status           VARCHAR(10) NOT NULL DEFAULT 'PROPOSED',
    source_class     VARCHAR(24) NOT NULL,
    valid_from       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    valid_to         TIMESTAMP WITH TIME ZONE,
    evidence_ref     TEXT,
    created_by       VARCHAR(255) NOT NULL,
    verified_by      VARCHAR(255),
    verified_at      TIMESTAMP WITH TIME ZONE,
    retired_by       VARCHAR(255),
    retired_at       TIMESTAMP WITH TIME ZONE,
    retire_reason    TEXT,
    correlation_id   VARCHAR(255),
    record_version   BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT pcp_party_in_tenant FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT pcp_channel_known CHECK (channel IN ('EMAIL')),
    CONSTRAINT pcp_purpose_known CHECK (purpose IN ('GENERAL', 'BILLING', 'LEGAL_NOTICE')),
    CONSTRAINT pcp_status_known CHECK (status IN ('PROPOSED', 'VERIFIED', 'RETIRED')),
    CONSTRAINT pcp_source_known CHECK (source_class IN
        ('AUTHORITY_REGISTRY', 'AUTHORITY_DOCUMENT', 'TENANT_ASSERTED', 'SYSTEM_IMPORT', 'EXTERNAL_PROVIDER')),
    CONSTRAINT pcp_value_present CHECK (btrim(value) <> '' AND char_length(value) <= 254),
    CONSTRAINT pcp_digest_hex CHECK (value_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT pcp_interval CHECK (valid_to IS NULL OR valid_from < valid_to),
    CONSTRAINT pcp_version_positive CHECK (record_version > 0),
    -- Maker-checker: the principal who proposed a contact point does not verify it.
    CONSTRAINT pcp_independent_verification CHECK (verified_by IS NULL OR verified_by <> created_by),
    CONSTRAINT pcp_verified_pair CHECK ((verified_by IS NULL) = (verified_at IS NULL)),
    CONSTRAINT pcp_verified_needs_actor CHECK (status <> 'VERIFIED' OR verified_by IS NOT NULL),
    CONSTRAINT pcp_verified_needs_evidence CHECK (verified_by IS NULL OR btrim(COALESCE(evidence_ref, '')) <> ''),
    CONSTRAINT pcp_proposed_unverified CHECK (status <> 'PROPOSED' OR (verified_by IS NULL AND retired_by IS NULL AND valid_to IS NULL)),
    CONSTRAINT pcp_retired_complete CHECK ((status = 'RETIRED') = (retired_by IS NOT NULL AND retired_at IS NOT NULL
        AND valid_to IS NOT NULL AND btrim(COALESCE(retire_reason, '')) <> ''))
);

-- One live contact point per party, channel and address.
CREATE UNIQUE INDEX pcp_one_live_value ON party_contact_points (tenant_id, party_id, channel, value_digest) WHERE status <> 'RETIRED';
CREATE INDEX idx_pcp_party ON party_contact_points (tenant_id, party_id, channel, status);

-- Party, channel, address and origin never change; status moves PROPOSED to VERIFIED or RETIRED, VERIFIED to RETIRED, and
-- RETIRED is final. Nothing is deleted.
CREATE OR REPLACE FUNCTION party_contact_point_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'party contact points are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.contact_point_id <> OLD.contact_point_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.party_id <> OLD.party_id
       OR NEW.channel <> OLD.channel OR NEW.value <> OLD.value OR NEW.value_digest <> OLD.value_digest
       OR NEW.purpose <> OLD.purpose OR NEW.source_class <> OLD.source_class OR NEW.valid_from <> OLD.valid_from
       OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a contact point is immutable apart from its status' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired contact point is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (NEW.status = OLD.status OR (OLD.status = 'PROPOSED' AND NEW.status IN ('VERIFIED', 'RETIRED'))
            OR (OLD.status = 'VERIFIED' AND NEW.status = 'RETIRED')) THEN
        RAISE EXCEPTION 'a contact point cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.verified_by IS NOT NULL AND (NEW.verified_by IS DISTINCT FROM OLD.verified_by OR NEW.verified_at IS DISTINCT FROM OLD.verified_at) THEN
        RAISE EXCEPTION 'a verification is not rewritten' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER party_contact_point_guard BEFORE UPDATE OR DELETE ON party_contact_points
    FOR EACH ROW EXECUTE FUNCTION party_contact_point_guard();

ALTER TABLE party_contact_points ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_contact_points FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON party_contact_points FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
