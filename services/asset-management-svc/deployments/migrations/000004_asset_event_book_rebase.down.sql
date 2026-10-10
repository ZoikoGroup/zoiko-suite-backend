DROP TABLE IF EXISTS asset_event_schedule_effects;
DROP FUNCTION IF EXISTS reject_asset_event_effect_mutation();

CREATE OR REPLACE FUNCTION reject_asset_event_economic_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('APPLIED', 'ACCOUNTING_EVENT_EMITTED', 'REVERSED', 'SUPERSEDED') AND (
        NEW.event_type IS DISTINCT FROM OLD.event_type OR
        NEW.asset_id IS DISTINCT FROM OLD.asset_id OR
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

ALTER TABLE asset_events DROP COLUMN IF EXISTS book_id;
