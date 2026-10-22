-- Migration: 000006_pair_closed_state_transitions.up.sql
--
-- SPEC_DEVIATIONS A9. ACC-11's state model ends "Resolved/Closed", but only
-- RESOLVED existed (reachable only from DISPUTED), a MATCHED pair had no
-- terminal close, and a resolution did not record whether the mismatch was
-- accepted, corrected or written off, so consolidation cannot tell whether a
-- RESOLVED pair may be eliminated.
--
-- Adds:
--   * CLOSED, the terminal state of a MATCHED pair (e.g. after its period is
--     consolidated), with who closed it, when and why;
--   * resolution_outcome on RESOLVED (ACCEPTED / CORRECTED / WRITTEN_OFF);
--   * intercompany_pair_transitions, the append-only record of every state
--     change, the same pattern as reconciliation-engine-svc's
--     reconciliation_run_transitions (its migration 000006): never updated,
--     never deleted, enforced by trigger, not by convention.

ALTER TABLE intercompany_entries
    ADD COLUMN resolution_outcome     VARCHAR(16)
        CONSTRAINT chk_resolution_outcome CHECK (resolution_outcome IN ('ACCEPTED', 'CORRECTED', 'WRITTEN_OFF')),
    ADD COLUMN closed_at              TIMESTAMP WITH TIME ZONE,
    ADD COLUMN closed_by_principal_id VARCHAR(255),
    ADD COLUMN close_reason           TEXT;

ALTER TABLE intercompany_entries DROP CONSTRAINT chk_match_status;
ALTER TABLE intercompany_entries
    ADD CONSTRAINT chk_match_status CHECK (match_status IN (
        'UNMATCHED', 'AWAITING_COUNTERPARTY', 'MATCHED', 'MISMATCH', 'DISPUTED', 'RESOLVED', 'CLOSED'
    )),
    ADD CONSTRAINT chk_closed_is_attributed CHECK (
        match_status <> 'CLOSED'
        OR (closed_at IS NOT NULL AND closed_by_principal_id IS NOT NULL AND close_reason IS NOT NULL)
    );

-- Every resolution from now on states its outcome. NOT VALID: pairs resolved
-- before this migration recorded none, and one is not invented for them;
-- they stay RESOLVED with a NULL outcome, which a consumer must read as
-- "unknown", never as eliminable. RESOLVED is terminal, so no later write
-- to such a row can trip the constraint.
ALTER TABLE intercompany_entries
    ADD CONSTRAINT chk_resolved_has_outcome CHECK (
        match_status <> 'RESOLVED' OR resolution_outcome IS NOT NULL
    ) NOT VALID;

CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE intercompany_pair_transitions (
    transition_id         BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id             VARCHAR(255) NOT NULL,
    intercompany_entry_id UUID         NOT NULL REFERENCES intercompany_entries (intercompany_entry_id),
    -- NULL only on the first row of a pair: its creation, or the state it was
    -- already in when this migration ran.
    from_state            VARCHAR(50),
    to_state              VARCHAR(50)  NOT NULL,
    -- The pair's period at the time of the change.
    fiscal_period         VARCHAR(20),
    reason                TEXT,
    resolution_outcome    VARCHAR(16),
    actor                 VARCHAR(255) NOT NULL,
    correlation_id        VARCHAR(255),
    occurred_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (from_state IS DISTINCT FROM to_state),
    CHECK (to_state IN ('UNMATCHED', 'AWAITING_COUNTERPARTY', 'MATCHED', 'MISMATCH', 'DISPUTED', 'RESOLVED', 'CLOSED')),
    CHECK (resolution_outcome IS NULL OR (to_state = 'RESOLVED'
        AND resolution_outcome IN ('ACCEPTED', 'CORRECTED', 'WRITTEN_OFF')))
);
CREATE INDEX idx_intercompany_pair_transitions_pair
    ON intercompany_pair_transitions (tenant_id, intercompany_entry_id, transition_id);

-- Backfill: one row per existing pair, recording the state it is in now. The
-- transitions that led there were never recorded and are not reconstructed;
-- the actor 'migration:000006' tells a reader this row from a live change.
INSERT INTO intercompany_pair_transitions
    (tenant_id, intercompany_entry_id, from_state, to_state, fiscal_period, reason, actor)
SELECT tenant_id, intercompany_entry_id, NULL, match_status, fiscal_period,
       'state as of migration 000006; earlier transitions were not recorded', 'migration:000006'
FROM intercompany_entries;

-- After the backfill: under FORCE, the backfill would itself need a tenant
-- context.
ALTER TABLE intercompany_pair_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE intercompany_pair_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY intercompany_pair_transitions_tenant_isolation ON intercompany_pair_transitions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- History is never rewritten.
CREATE TRIGGER trigger_pair_transitions_immutable
    BEFORE UPDATE OR DELETE ON intercompany_pair_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();
