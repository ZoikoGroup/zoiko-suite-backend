-- 000021_identifier_verification.up.sql
--
-- ORG-09 / MDM-03 identifier verification (ZS-SVC-I-001 s4.9, ZS-SVC-U-001 s13.2, s13.3, s14.1).
--
-- A verification is an observation of what an authority or provider said about an identifier, recorded with its
-- source, version, time and evidence. It does not certify future validity and it is never last-write-wins: a source
-- is applied by precedence and freshness, and a higher-authority source that disagrees with a verified identifier
-- opens a stewardship case and leaves the identifier CONFLICTED instead of overwriting it.

-- 1. What verified an identifier, and when.
ALTER TABLE party_identifiers ADD CONSTRAINT pi_tenant_unique UNIQUE (tenant_id, identifier_id);
ALTER TABLE party_identifiers ADD COLUMN verified_source_class VARCHAR(24);
ALTER TABLE party_identifiers ADD COLUMN verified_at TIMESTAMP WITH TIME ZONE;
-- The time of the last observation that changed the identifier's state: an older observation never overrides it.
ALTER TABLE party_identifiers ADD COLUMN last_observed_at TIMESTAMP WITH TIME ZONE;

-- An identifier linked as VERIFIED from an authority (s10.1) was verified by that source when it was linked. Existing
-- rows are backfilled, and a trigger fills the same for every later insert, so no writer has to know.
UPDATE party_identifiers SET verified_source_class = source_class, verified_at = linked_at WHERE verification_status = 'VERIFIED';
CREATE OR REPLACE FUNCTION party_identifier_verified_source() RETURNS trigger AS $$
BEGIN
    IF NEW.verification_status IN ('VERIFIED', 'CONFLICTED') AND NEW.verified_source_class IS NULL THEN
        NEW.verified_source_class := NEW.source_class;
        NEW.verified_at := COALESCE(NEW.verified_at, NEW.linked_at);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER party_identifier_verified_source BEFORE INSERT OR UPDATE ON party_identifiers
    FOR EACH ROW EXECUTE FUNCTION party_identifier_verified_source();

ALTER TABLE party_identifiers DROP CONSTRAINT pi_verification_known;
ALTER TABLE party_identifiers ADD CONSTRAINT pi_verification_known CHECK (verification_status IN
    ('UNVERIFIED', 'VERIFIED', 'FAILED', 'CONFLICTED'));
-- Verified, or verified and now disputed, always says by what.
ALTER TABLE party_identifiers ADD CONSTRAINT pi_verified_has_source CHECK (
    verification_status NOT IN ('VERIFIED', 'CONFLICTED') OR (verified_source_class IS NOT NULL AND verified_at IS NOT NULL));

-- 2. The observations: append-only.
CREATE TABLE identifier_verifications (
    observation_id   UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL,
    identifier_id    UUID NOT NULL,
    party_id         UUID NOT NULL,
    scheme           VARCHAR(24) NOT NULL,
    result           VARCHAR(12) NOT NULL,
    source_class     VARCHAR(24) NOT NULL,
    -- The authority or provider that answered, its data version, and the exchange that carried it (the XIC reference).
    source_ref       TEXT NOT NULL,
    source_version   TEXT,
    exchange_ref     TEXT,
    observed_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    -- LEI: the registration status the LOU reports (ISSUED, LAPSED, ...).
    authority_status VARCHAR(24),
    evidence_ref     TEXT NOT NULL,
    evidence_hash    VARCHAR(64),
    -- What recording it did: VERIFIED, SOURCE_UPGRADED, FAILED, CONFLICT_OPENED or NO_CHANGE, and why.
    effect           VARCHAR(16) NOT NULL,
    effect_note      TEXT NOT NULL,
    case_id          UUID,
    status_before    VARCHAR(12) NOT NULL,
    status_after     VARCHAR(12) NOT NULL,
    recorded_by      VARCHAR(255) NOT NULL,
    recorded_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id   VARCHAR(255),
    CONSTRAINT iv_identifier_in_tenant FOREIGN KEY (tenant_id, identifier_id) REFERENCES party_identifiers (tenant_id, identifier_id),
    CONSTRAINT iv_result_known CHECK (result IN ('MATCH', 'MISMATCH', 'NOT_FOUND')),
    CONSTRAINT iv_source_known CHECK (source_class IN
        ('AUTHORITY_REGISTRY', 'AUTHORITY_DOCUMENT', 'TENANT_ASSERTED', 'SYSTEM_IMPORT', 'EXTERNAL_PROVIDER')),
    CONSTRAINT iv_effect_known CHECK (effect IN ('VERIFIED', 'SOURCE_UPGRADED', 'FAILED', 'CONFLICT_OPENED', 'NO_CHANGE')),
    CONSTRAINT iv_present CHECK (btrim(source_ref) <> '' AND btrim(evidence_ref) <> '' AND btrim(effect_note) <> ''),
    CONSTRAINT iv_hash_shape CHECK (evidence_hash IS NULL OR evidence_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT iv_conflict_has_case CHECK ((effect = 'CONFLICT_OPENED') = (case_id IS NOT NULL))
);
CREATE INDEX idx_iv_identifier ON identifier_verifications (tenant_id, identifier_id, recorded_at);

CREATE OR REPLACE FUNCTION identifier_verification_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'identifier verifications are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER identifier_verification_guard BEFORE UPDATE OR DELETE ON identifier_verifications
    FOR EACH ROW EXECUTE FUNCTION identifier_verification_guard();

ALTER TABLE identifier_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE identifier_verifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON identifier_verifications FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

-- 3. A higher-authority source that disagrees with a verified identifier opens a case a steward decides.
ALTER TABLE resolution_cases ADD COLUMN identifier_id UUID;
ALTER TABLE resolution_cases ADD COLUMN observation_id UUID;
ALTER TABLE resolution_cases DROP CONSTRAINT rc_type_known;
ALTER TABLE resolution_cases ADD CONSTRAINT rc_type_known CHECK (case_type IN
    ('DUPLICATE_REVIEW', 'IDENTIFIER_CONFLICT', 'FACT_CONFLICT', 'VERIFICATION_CONFLICT'));
ALTER TABLE resolution_cases DROP CONSTRAINT rc_decision_known;
ALTER TABLE resolution_cases ADD CONSTRAINT rc_decision_known CHECK (decision IS NULL OR decision IN
    ('MERGE', 'CREATE_NEW', 'LINK_EXISTING', 'ACCEPT_PROPOSED_FACT', 'KEEP_VERIFIED', 'KEEP_HOLDER', 'MOVE_TO_REQUESTER', 'DISMISS',
     'ACCEPT_OBSERVATION'));
