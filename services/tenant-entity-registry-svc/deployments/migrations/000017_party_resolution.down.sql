-- 000017_party_resolution.down.sql
-- A merge is a decision about who a party is. Reversing the schema under one would lose the alias, so it is
-- refused while any lineage or decided case exists.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM party_lineage) OR EXISTS (SELECT 1 FROM resolution_cases WHERE status <> 'OPEN') THEN
        RAISE EXCEPTION '000017 down refused: merge lineage or decided cases exist and are evidence';
    END IF;
END
$$;

DROP TABLE IF EXISTS party_lineage;
DROP FUNCTION IF EXISTS party_lineage_guard();

ALTER TABLE resolution_cases DROP CONSTRAINT rc_independent_decision;
ALTER TABLE resolution_cases DROP CONSTRAINT rc_decided_consistent;
ALTER TABLE resolution_cases DROP CONSTRAINT rc_decision_known;
ALTER TABLE resolution_cases DROP COLUMN proposed_fact_id;
ALTER TABLE resolution_cases DROP COLUMN resolved_party_id;
ALTER TABLE resolution_cases DROP COLUMN decision_note;
ALTER TABLE resolution_cases DROP COLUMN decision;
ALTER TABLE resolution_cases DROP COLUMN decided_at;
ALTER TABLE resolution_cases DROP COLUMN decided_by;

ALTER TABLE party_facts DROP CONSTRAINT pf_status_known;
ALTER TABLE party_facts ADD CONSTRAINT pf_status_known CHECK (status IN ('PROPOSED', 'VERIFIED', 'SUPERSEDED', 'REJECTED'));

ALTER TABLE party_identifiers DROP CONSTRAINT pi_end_consistent;
ALTER TABLE party_identifiers DROP COLUMN moved_from_identifier_id;
ALTER TABLE party_identifiers DROP COLUMN ended_reason;
ALTER TABLE party_identifiers DROP COLUMN ended_at;

DROP INDEX IF EXISTS idx_parties_merged_into;
ALTER TABLE parties DROP CONSTRAINT party_merged_pair;
ALTER TABLE parties DROP CONSTRAINT party_merged_into_tenant;
ALTER TABLE parties DROP COLUMN merged_into_party_id;
ALTER TABLE parties DROP CONSTRAINT party_status_known;
ALTER TABLE parties ADD CONSTRAINT party_status_known CHECK (status IN ('CANDIDATE', 'ACTIVE', 'INACTIVE'));
