-- Migration: 000004_asset_event_book_rebase.up.sql
--
-- AST-03 follow-on: IMPAIRMENT, REVALUATION and ADDITION events now change
-- book state (spec invariant 2: book-specific values). Migration 000003
-- recorded them as history only; this migration adds what is needed for
-- ApplyAssetEvent to re-base the asset's current depreciation schedule.
--
-- 1. asset_events.book_id (nullable). REQUIRED, at ValidateAssetEvent, for
--    IMPAIRMENT / REVALUATION / ADDITION; it names the book whose ACTIVE
--    depreciation schedule is re-based. Nullable at the column level
--    because the other event types are not book-specific and because rows
--    recorded before this migration have none. book_id is part of an
--    applied event's immutable economic substance (trigger recreated below).
--
-- 2. Amount semantics (applies to the re-base, see asset_event_rebase.go):
--      IMPAIRMENT   amount = the impairment LOSS (carrying decreases by it)
--      ADDITION     amount = the capitalised ADDITION (carrying increases)
--      REVALUATION  amount = the REVALUED CARRYING AMOUNT itself (not a
--                   delta); the new schedule version's cost_basis = amount.
--
-- 3. asset_event_schedule_effects: insert-only link from an applied (or
--    reversed) event to the old and new schedule versions it produced,
--    with the carrying amounts and remaining months used. A re-base is
--    "end-date the current ACTIVE version, insert a new version whose
--    cost_basis is the new carrying amount and whose useful_life_months is
--    the remaining months" - depreciation_lines are keyed by
--    schedule_version_id so the new version starts at accumulated = 0.
--    effect_kind is APPLY or REVERSE; one row per (event, kind).
ALTER TABLE asset_events ADD COLUMN book_id VARCHAR(255);

CREATE OR REPLACE FUNCTION reject_asset_event_economic_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('APPLIED', 'ACCOUNTING_EVENT_EMITTED', 'REVERSED', 'SUPERSEDED') AND (
        NEW.event_type IS DISTINCT FROM OLD.event_type OR
        NEW.asset_id IS DISTINCT FROM OLD.asset_id OR
        NEW.book_id IS DISTINCT FROM OLD.book_id OR
        NEW.amount IS DISTINCT FROM OLD.amount OR
        NEW.proceeds_amount IS DISTINCT FROM OLD.proceeds_amount OR
        NEW.valuation_evidence_ref IS DISTINCT FROM OLD.valuation_evidence_ref OR
        NEW.source_document_ref IS DISTINCT FROM OLD.source_document_ref OR
        NEW.effective_date IS DISTINCT FROM OLD.effective_date OR
        NEW.currency IS DISTINCT FROM OLD.currency
    ) THEN
        RAISE EXCEPTION 'asset_events: economic fields of an applied event are immutable — use ReverseAssetEvent/SupersedeAssetEvent, not UPDATE';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE asset_event_schedule_effects (
    effect_id                  UUID PRIMARY KEY,
    event_id                     UUID NOT NULL REFERENCES asset_events(event_id),
    effect_kind                    VARCHAR(10) NOT NULL,
    tenant_id                        VARCHAR(255) NOT NULL,
    old_schedule_version_id            UUID NOT NULL REFERENCES depreciation_schedules(schedule_version_id),
    new_schedule_version_id              UUID NOT NULL REFERENCES depreciation_schedules(schedule_version_id),
    old_carrying                           NUMERIC(18,2) NOT NULL,
    new_carrying                             NUMERIC(18,2) NOT NULL,
    remaining_months                           INT NOT NULL,
    created_at                                   TIMESTAMP WITH TIME ZONE NOT NULL,

    CONSTRAINT chk_asset_event_effect_kind CHECK (effect_kind IN ('APPLY', 'REVERSE')),
    CONSTRAINT uq_asset_event_effect UNIQUE (event_id, effect_kind)
);

CREATE OR REPLACE FUNCTION reject_asset_event_effect_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'asset_event_schedule_effects is insert-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_asset_event_effect_update
    BEFORE UPDATE ON asset_event_schedule_effects
    FOR EACH ROW EXECUTE FUNCTION reject_asset_event_effect_mutation();
CREATE TRIGGER trg_reject_asset_event_effect_delete
    BEFORE DELETE ON asset_event_schedule_effects
    FOR EACH ROW EXECUTE FUNCTION reject_asset_event_effect_mutation();

ALTER TABLE asset_event_schedule_effects ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_event_schedule_effects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON asset_event_schedule_effects
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_asset_event_effects_event ON asset_event_schedule_effects (tenant_id, event_id);
