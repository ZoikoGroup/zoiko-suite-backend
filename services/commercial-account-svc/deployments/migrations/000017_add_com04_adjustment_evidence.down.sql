-- 000017_add_com04_adjustment_evidence.down.sql
ALTER TABLE usage_adjustments DROP COLUMN IF EXISTS occurred_at;
ALTER TABLE usage_adjustments DROP COLUMN IF EXISTS dimensions;
