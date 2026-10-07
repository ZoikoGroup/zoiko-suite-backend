-- Migration: 000013_add_open_item_crosswalk_type.up.sql
--
-- ACC-17 (Opening Balance & Migration) gap closure. Migration 000009's own
-- doc comment already named the shortfall honestly: the UNIQUE(batch_id,
-- source_reference_id) constraint is "the closest real enforcement this v1
-- has of the spec's own negative path, 'Open AR included both in history
-- and opening state'" — real, but only catches a duplicate WITHIN one
-- migration batch, never a source item that ALSO already exists as a real,
-- live invoice in accounts-receivable-svc's (or accounts-payable-svc's)
-- own history.
--
-- source_reference_type and party_id let a caller flag which crosswalk
-- entries represent an open AR/AP item (as opposed to an ordinary GL
-- balance line, which needs no such cross-check) and which
-- customer/vendor it belongs to — ValidateOpeningBalances uses both to
-- call the real subledger service and refuse a batch that would
-- double-book an item the subledger already has. Both nullable: a plain
-- GL balance crosswalk entry needs neither.
ALTER TABLE migration_crosswalk_entries
    ADD COLUMN source_reference_type VARCHAR(20), -- AR_OPEN_ITEM | AP_OPEN_ITEM | NULL
    ADD COLUMN party_id VARCHAR(255); -- customer_id or vendor_id, required only when source_reference_type is set

ALTER TABLE migration_crosswalk_entries
    ADD CONSTRAINT chk_crosswalk_source_reference_type
    CHECK (source_reference_type IS NULL OR source_reference_type IN ('AR_OPEN_ITEM', 'AP_OPEN_ITEM'));
