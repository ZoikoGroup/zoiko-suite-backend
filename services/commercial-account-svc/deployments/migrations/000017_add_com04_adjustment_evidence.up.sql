-- 000017_add_com04_adjustment_evidence.up.sql
-- COM-04 gap-remediation, following a skeptical re-audit that found the
-- late-usage "prospective adjustment" mechanism (COM-CTRL-017) computed and
-- stored an adjustment but never actually counted its quantity toward any
-- billed total: certifyStatement only read usage_adjustments to decide
-- CERTIFIED vs ADJUSTED status, never to add the quantity in. Late usage
-- was silently never billed.
--
-- usage_adjustments needs occurred_at/dimensions to participate in
-- domain.Aggregate exactly like a normal accepted event (SUM/MAX/LAST all
-- need occurred_at for tie-breaking or ordering; UNIQUE_COUNT needs the
-- dimension value) — a bare quantity was only ever enough for SUM, and even
-- that was never actually read. Nullable-then-backfill-then-NOT-NULL: this
-- table may already hold rows from before this fix (created via the
-- automatic late-routing path, which had no way to supply these values),
-- not an assumption that it's empty.

ALTER TABLE usage_adjustments ADD COLUMN occurred_at TIMESTAMPTZ;
ALTER TABLE usage_adjustments ADD COLUMN dimensions JSONB;

UPDATE usage_adjustments SET occurred_at = created_at WHERE occurred_at IS NULL;
UPDATE usage_adjustments SET dimensions = '{}'::jsonb WHERE dimensions IS NULL;

ALTER TABLE usage_adjustments ALTER COLUMN occurred_at SET NOT NULL;
ALTER TABLE usage_adjustments ALTER COLUMN dimensions SET NOT NULL;
ALTER TABLE usage_adjustments ALTER COLUMN dimensions SET DEFAULT '{}'::jsonb;
