-- +migrate Up
BEGIN;

-- LEG-04 (docs/architecture/original_doc) §6 names GetVoterPopulation and
-- GetVoteLedger as required read surfaces and states the mandatory
-- invariant: "Quorum and voter eligibility are evaluated against a frozen
-- as-of entitlement population." Neither existed — RecordVotes accepted
-- three bare integers from any authorized caller and PassResolution never
-- checked them against anything. This migration adds the two tables that
-- make a real quorum check possible.
--
-- A full spec-correct roster would be sourced from LEG-02 (director
-- register) or LEG-03 (shareholder register) as of the moment voting opens.
-- Neither of those sub-services exists anywhere in this repo yet, so the
-- roster here is explicitly caller-asserted via OpenVoting, not
-- auto-sourced. That is a known, named gap — not a silent approximation —
-- to be closed once LEG-02/03 exist.

CREATE TABLE IF NOT EXISTS board_resolution_voter_roster (
    resolution_id      TEXT        NOT NULL,
    tenant_id           TEXT        NOT NULL,
    voter_principal_id TEXT        NOT NULL,
    frozen_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (resolution_id, tenant_id, voter_principal_id)
);

-- One row per voter per resolution. The primary key is the dedup: a second
-- CastVote for the same (resolution, voter) is rejected by the store before
-- it reaches this table, not overwritten — a vote, once cast, is evidence,
-- not a mutable field (LEG-04 Evidence/retention: "votes/consents... final
-- text digest" are retained, and a silently-replaced vote would make that
-- retained record false).
CREATE TABLE IF NOT EXISTS board_resolution_votes (
    resolution_id      TEXT        NOT NULL,
    tenant_id           TEXT        NOT NULL,
    voter_principal_id TEXT        NOT NULL,
    vote                TEXT        NOT NULL,
    cast_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    cast_by             TEXT        NOT NULL,
    PRIMARY KEY (resolution_id, tenant_id, voter_principal_id)
);

ALTER TABLE board_resolution_voter_roster ENABLE ROW LEVEL SECURITY;
ALTER TABLE board_resolution_voter_roster FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS voter_roster_tenant_isolation ON board_resolution_voter_roster;
CREATE POLICY voter_roster_tenant_isolation ON board_resolution_voter_roster
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE board_resolution_votes ENABLE ROW LEVEL SECURITY;
ALTER TABLE board_resolution_votes FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS votes_tenant_isolation ON board_resolution_votes;
CREATE POLICY votes_tenant_isolation ON board_resolution_votes
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE board_resolution_votes
    ADD CONSTRAINT board_resolution_votes_vote_known
    CHECK (vote IN ('FOR', 'AGAINST', 'ABSTAIN'));

CREATE INDEX IF NOT EXISTS idx_voter_roster_resolution ON board_resolution_voter_roster (tenant_id, resolution_id);
CREATE INDEX IF NOT EXISTS idx_votes_resolution ON board_resolution_votes (tenant_id, resolution_id);

-- LEG-04 §Lifecycle: Draft -> Proposed -> Open -> Passed/Failed -> Executed
-- -> Effective -> Superseded/Archived. This pass implements the quorum-
-- bearing core of that (Proposed -> Open -> Passed/Failed -> Superseded);
-- Draft, Executed, Effective and Archived are deferred (see handler.go) --
-- a passed resolution emits authorization evidence only (invariant 6.1),
-- so Executed/Effective describe a downstream action this service does not
-- perform, and Draft/Archived are bookkeeping states with no bearing on the
-- quorum-enforcement gap this migration exists to close.
ALTER TABLE board_resolutions DROP CONSTRAINT IF EXISTS board_resolutions_status_known;
ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_status_known
    CHECK (status IN ('PROPOSED', 'OPEN', 'PASSED', 'FAILED', 'SUPERSEDED')) NOT VALID;

-- quorum_threshold is frozen at OpenVoting time alongside the roster: the
-- minimum number of roster members who must cast a vote (FOR/AGAINST/ABSTAIN
-- all count toward quorum; only FOR vs AGAINST decide the outcome) for
-- CloseVoting to resolve PASSED/FAILED rather than remaining OPEN.
ALTER TABLE board_resolutions ADD COLUMN IF NOT EXISTS quorum_threshold INTEGER;
ALTER TABLE board_resolutions ADD COLUMN IF NOT EXISTS voting_opened_at TIMESTAMPTZ;
ALTER TABLE board_resolutions ADD COLUMN IF NOT EXISTS voting_closed_at TIMESTAMPTZ;
ALTER TABLE board_resolutions ADD COLUMN IF NOT EXISTS superseded_by TEXT;

ALTER TABLE board_resolutions
    ADD CONSTRAINT board_resolutions_open_has_quorum
    CHECK (status = 'PROPOSED' OR quorum_threshold IS NOT NULL) NOT VALID;

COMMIT;
