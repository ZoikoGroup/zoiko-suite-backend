-- BNK-05 A2b part 2 (reconciliation-intelligence-svc's SPEC_DEVIATIONS):
-- ZS-CONTROL-001 section 10 requires "Many-to-one / one-to-many: Support
-- split payments, consolidated receipts, fees and settlements with explicit
-- allocation." statement_lines.matched_journal_id/proposed_journal_id are
-- scalar columns — exactly one journal per line — so a payment booked as
-- two or more journals could never be matched at all, on the journal path
-- (the deprecated one; the canonical-transaction path is unaffected and
-- still out of scope for this).
--
-- This is additive, not a replacement: the existing single-journal path
-- (matched_journal_id/proposed_journal_id) is unchanged for backward
-- compatibility. A line matched (or proposed) against two or more journals
-- leaves those scalar columns NULL and instead has rows here — the
-- "explicit allocation" evidence the spec requires, one row per journal
-- naming its own net cash movement, which is what verification actually
-- summed to equal the line's amount. A single table serves both lifecycle
-- phases: which phase a row set represents is read off the parent
-- statement_line's own status (PENDING_CONFIRMATION = proposed, MATCHED =
-- confirmed), the same way the existing scalar columns already work.
CREATE TABLE statement_line_matched_journals (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    statement_line_id UUID NOT NULL REFERENCES statement_lines (statement_line_id),
    journal_id        UUID NOT NULL,
    -- The journal's own signed net movement on the line's cash account, in
    -- the line's currency's minor units — verified at propose time and
    -- re-verified at confirm time, never trusted from the caller.
    net_amount_cents  BIGINT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one row per (line, journal): a journal cannot be named twice in
-- the same allocation.
CREATE UNIQUE INDEX idx_statement_line_matched_journals_line_journal
    ON statement_line_matched_journals (statement_line_id, journal_id);
CREATE INDEX idx_statement_line_matched_journals_tenant
    ON statement_line_matched_journals (tenant_id);

ALTER TABLE statement_line_matched_journals ENABLE ROW LEVEL SECURITY;
ALTER TABLE statement_line_matched_journals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON statement_line_matched_journals
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::UUID)
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::UUID);

-- A row is written once at propose time and never edited — a correction is
-- reject-then-repropose, the same pattern the existing scalar
-- proposed_journal_id column already follows. Deletion is permitted only
-- while the parent line is still PENDING_CONFIRMATION (RejectProposedMatch
-- clears a withdrawn proposal); once the parent reaches MATCHED, this row
-- is confirmed evidence and is append-only, the same posture
-- reconciliation_certificates already has.
CREATE OR REPLACE FUNCTION reject_matched_journal_allocation_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'statement_line_matched_journals rows are never updated — reject and re-propose instead';
    END IF;
    -- TG_OP = 'DELETE'
    IF EXISTS (SELECT 1 FROM statement_lines WHERE statement_line_id = OLD.statement_line_id AND status = 'MATCHED') THEN
        RAISE EXCEPTION 'statement_line_matched_journals row % is part of a confirmed match and can never be deleted', OLD.id;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_matched_journal_allocation_mutation
    BEFORE UPDATE OR DELETE ON statement_line_matched_journals
    FOR EACH ROW EXECUTE FUNCTION reject_matched_journal_allocation_mutation();
