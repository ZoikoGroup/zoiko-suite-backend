-- Migration: 000003_add_acc11_lifecycle.up.sql
--
-- ACC-11 (Intercompany Accounting): "owns Intercompany pair/match state.
-- Must never own: Local entity source accounting." Fuller ownership:
-- "IntercompanyPair, reciprocal reference, matching/dispute state and
-- group counterparty classification." State model (verbatim): "Open →
-- AwaitingCounterparty → Matched / Mismatched / Disputed →
-- Resolved/Closed."
--
-- The pre-existing match_status column ('UNMATCHED', 'MATCHED',
-- 'MISMATCH') already covered three of the spec's six states — this adds
-- the rest to the SAME column (match_status IS the state model; adding a
-- parallel "pair_status" column would fork one lifecycle into two that
-- could disagree) rather than renaming it: 'UNMATCHED' is kept as the
-- literal string for the spec's "Open", so no existing row or caller
-- needs to change.
ALTER TABLE intercompany_entries
    ADD COLUMN acknowledged_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN acknowledged_by_principal_id VARCHAR(255),
    ADD COLUMN disputed_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN disputed_by_principal_id VARCHAR(255),
    ADD COLUMN dispute_reason TEXT,
    ADD COLUMN resolved_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN resolved_by_principal_id VARCHAR(255),
    ADD COLUMN resolution_note TEXT;

-- match_status was an unconstrained free-text column; naming the full
-- state set here catches a typo'd status the same way a closed enum
-- would, without introducing a Postgres ENUM type this codebase doesn't
-- use elsewhere.
ALTER TABLE intercompany_entries
    ADD CONSTRAINT chk_match_status CHECK (match_status IN (
        'UNMATCHED', 'AWAITING_COUNTERPARTY', 'MATCHED', 'MISMATCH', 'DISPUTED', 'RESOLVED'
    ));
