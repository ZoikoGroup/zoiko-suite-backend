-- 000017_party_resolution.up.sql
--
-- ORG-07 / MDM-05 merge, split and stewardship decisions (ZS-SVC-U-001 s18.2, s19).
--
-- A merge is a governed relationship change, not a database row merge: the survivor keeps its id, the duplicate
-- keeps its row (status MERGED, resolving to the survivor), and what was decided, by whom and why is an append-only
-- lineage. A split corrects a wrong merge by adding more lineage, never by erasing the error.

-- 1. A merged party is a party with a status and a place to resolve to.
ALTER TABLE parties DROP CONSTRAINT party_status_known;
ALTER TABLE parties ADD CONSTRAINT party_status_known CHECK (status IN ('CANDIDATE', 'ACTIVE', 'INACTIVE', 'MERGED'));
ALTER TABLE parties ADD COLUMN merged_into_party_id UUID;
ALTER TABLE parties ADD CONSTRAINT party_merged_into_tenant FOREIGN KEY (tenant_id, merged_into_party_id)
    REFERENCES parties (tenant_id, party_id);
-- MERGED and "resolves to a survivor" are the same fact, and a party cannot resolve to itself.
ALTER TABLE parties ADD CONSTRAINT party_merged_pair CHECK (
    (status = 'MERGED') = (merged_into_party_id IS NOT NULL) AND merged_into_party_id IS DISTINCT FROM party_id);
CREATE INDEX idx_parties_merged_into ON parties (tenant_id, merged_into_party_id) WHERE merged_into_party_id IS NOT NULL;

-- 1b. A proposed fact a steward accepts is PROMOTED: the accepted fact is written fresh, verified now, so knowledge
-- time stays true, and the proposal row stays as the record of what was proposed.
ALTER TABLE party_facts DROP CONSTRAINT pf_status_known;
ALTER TABLE party_facts ADD CONSTRAINT pf_status_known CHECK (status IN ('PROPOSED', 'VERIFIED', 'SUPERSEDED', 'REJECTED', 'PROMOTED'));

-- 2. An identifier that moves is ended, not edited; the copy on the survivor says where it came from.
ALTER TABLE party_identifiers ADD COLUMN ended_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE party_identifiers ADD COLUMN ended_reason VARCHAR(40);
ALTER TABLE party_identifiers ADD COLUMN moved_from_identifier_id UUID;
ALTER TABLE party_identifiers ADD CONSTRAINT pi_end_consistent CHECK (
    (status = 'ENDED') = (ended_at IS NOT NULL AND ended_reason IS NOT NULL));

-- 3. A case is decided once, by someone other than whoever opened it (s4.7: independent review).
ALTER TABLE resolution_cases ADD COLUMN decided_by VARCHAR(255);
ALTER TABLE resolution_cases ADD COLUMN decided_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE resolution_cases ADD COLUMN decision VARCHAR(24);
ALTER TABLE resolution_cases ADD COLUMN decision_note TEXT;
ALTER TABLE resolution_cases ADD COLUMN resolved_party_id UUID;
-- The fact a FACT_CONFLICT is about; the verified fact it contradicts is the one in force.
ALTER TABLE resolution_cases ADD COLUMN proposed_fact_id UUID;
ALTER TABLE resolution_cases ADD CONSTRAINT rc_decision_known CHECK (decision IS NULL OR decision IN
    ('MERGE', 'CREATE_NEW', 'LINK_EXISTING', 'ACCEPT_PROPOSED_FACT', 'KEEP_VERIFIED', 'KEEP_HOLDER', 'MOVE_TO_REQUESTER', 'DISMISS'));
ALTER TABLE resolution_cases ADD CONSTRAINT rc_decided_consistent CHECK (
    (status = 'OPEN' AND decided_by IS NULL AND decided_at IS NULL AND decision IS NULL)
    OR (status <> 'OPEN' AND decided_by IS NOT NULL AND decided_at IS NOT NULL AND decision IS NOT NULL));
ALTER TABLE resolution_cases ADD CONSTRAINT rc_independent_decision CHECK (decided_by IS NULL OR decided_by <> opened_by);

-- 4. Merge and split lineage: append-only. One row per absorbed party.
CREATE TABLE party_lineage (
    lineage_id     UUID PRIMARY KEY,
    tenant_id      UUID NOT NULL REFERENCES tenants(tenant_id),
    event_type     VARCHAR(8) NOT NULL,
    case_id        UUID NOT NULL,
    survivor_id    UUID NOT NULL,
    party_id       UUID NOT NULL,
    actor          VARCHAR(255) NOT NULL,
    -- Whoever this decision had to be independent of: the case opener for a merge, the merger for a split.
    independent_of VARCHAR(255) NOT NULL,
    reason         TEXT NOT NULL,
    evidence_ref   TEXT NOT NULL,
    occurred_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    correlation_id VARCHAR(255),
    -- previous status, identifiers moved, roles consolidated, legal entity adopted: what a split needs to undo.
    details        JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT pl_survivor_in_tenant FOREIGN KEY (tenant_id, survivor_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT pl_party_in_tenant FOREIGN KEY (tenant_id, party_id) REFERENCES parties (tenant_id, party_id),
    CONSTRAINT pl_case_in_tenant FOREIGN KEY (tenant_id, case_id) REFERENCES resolution_cases (tenant_id, case_id),
    CONSTRAINT pl_type_known CHECK (event_type IN ('MERGE', 'SPLIT')),
    CONSTRAINT pl_distinct CHECK (survivor_id <> party_id),
    CONSTRAINT pl_independent CHECK (actor <> independent_of),
    CONSTRAINT pl_reason_and_evidence CHECK (btrim(reason) <> '' AND btrim(evidence_ref) <> '')
);
CREATE INDEX idx_pl_party ON party_lineage (tenant_id, party_id, occurred_at);
CREATE INDEX idx_pl_survivor ON party_lineage (tenant_id, survivor_id, occurred_at);

CREATE OR REPLACE FUNCTION party_lineage_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'party lineage is append-only: a wrong merge is corrected by a split, never erased'
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER party_lineage_guard BEFORE UPDATE OR DELETE ON party_lineage
    FOR EACH ROW EXECUTE FUNCTION party_lineage_guard();

ALTER TABLE party_lineage ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_lineage FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON party_lineage FOR ALL
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);
