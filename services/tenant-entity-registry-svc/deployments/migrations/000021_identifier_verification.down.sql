-- 000021_identifier_verification.down.sql
-- Verification observations are evidence, and a VERIFICATION_CONFLICT case or an ACCEPT_OBSERVATION decision cannot be
-- expressed without this migration. Refused while any exist.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM identifier_verifications)
       OR EXISTS (SELECT 1 FROM resolution_cases WHERE case_type = 'VERIFICATION_CONFLICT' OR decision = 'ACCEPT_OBSERVATION')
       OR EXISTS (SELECT 1 FROM party_identifiers WHERE verification_status = 'CONFLICTED') THEN
        RAISE EXCEPTION '000021 down refused: identifier verifications, verification conflicts or CONFLICTED identifiers exist';
    END IF;
END
$$;

ALTER TABLE resolution_cases DROP CONSTRAINT rc_decision_known;
ALTER TABLE resolution_cases ADD CONSTRAINT rc_decision_known CHECK (decision IS NULL OR decision IN
    ('MERGE', 'CREATE_NEW', 'LINK_EXISTING', 'ACCEPT_PROPOSED_FACT', 'KEEP_VERIFIED', 'KEEP_HOLDER', 'MOVE_TO_REQUESTER', 'DISMISS'));
ALTER TABLE resolution_cases DROP CONSTRAINT rc_type_known;
ALTER TABLE resolution_cases ADD CONSTRAINT rc_type_known CHECK (case_type IN ('DUPLICATE_REVIEW', 'IDENTIFIER_CONFLICT', 'FACT_CONFLICT'));
ALTER TABLE resolution_cases DROP COLUMN observation_id;
ALTER TABLE resolution_cases DROP COLUMN identifier_id;

DROP TABLE IF EXISTS identifier_verifications;
DROP FUNCTION IF EXISTS identifier_verification_guard();

ALTER TABLE party_identifiers DROP CONSTRAINT pi_verified_has_source;
ALTER TABLE party_identifiers DROP CONSTRAINT pi_verification_known;
ALTER TABLE party_identifiers ADD CONSTRAINT pi_verification_known CHECK (verification_status IN ('UNVERIFIED', 'VERIFIED', 'FAILED'));
DROP TRIGGER IF EXISTS party_identifier_verified_source ON party_identifiers;
DROP FUNCTION IF EXISTS party_identifier_verified_source();
ALTER TABLE party_identifiers DROP COLUMN last_observed_at;
ALTER TABLE party_identifiers DROP COLUMN verified_at;
ALTER TABLE party_identifiers DROP COLUMN verified_source_class;
ALTER TABLE party_identifiers DROP CONSTRAINT pi_tenant_unique;
