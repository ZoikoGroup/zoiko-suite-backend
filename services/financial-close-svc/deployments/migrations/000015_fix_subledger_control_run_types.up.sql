-- Migration: 000015_fix_subledger_control_run_types.up.sql
--
-- 1. subledger was VARCHAR(10) (000004, written when only AP and AR
--    existed). RunSubledgerControl has since accepted six more types, five of
--    which are longer than ten characters — DEPRECIATION_COMPLETENESS,
--    INVENTORY_QUANTITY, INVENTORY_VALUE, PROJECT_REVENUE, STOCK_COUNT — so
--    every one of those runs failed at INSERT with "value too long". The
--    handler tests use a stub store and never saw it. Widened, and limited to
--    the known types so a typo fails here instead of producing a run no close
--    requirement will ever match.
--
-- 2. An ASSETS run reconciles ONE asset book's net book value to the GL, but
--    the book it was asked about was used for the calculation and then
--    dropped: a stored run could not say what it had proven. book_id now
--    records it, and is required for ASSETS and absent for everything else.
--    The constraint is NOT VALID: an ASSETS run stored before this migration
--    has no book and cannot be given one (the table is append-only evidence),
--    so only new rows are held to it.
--
-- Widening a VARCHAR is a catalog-only change in Postgres: no table rewrite,
-- and the append-only UPDATE trigger does not fire.

ALTER TABLE subledger_control_runs ALTER COLUMN subledger TYPE VARCHAR(32);

ALTER TABLE subledger_control_runs ADD CONSTRAINT subledger_control_runs_subledger_known
    CHECK (subledger IN ('AP', 'AR', 'ASSETS', 'DEPRECIATION_COMPLETENESS', 'INVENTORY_QUANTITY',
                         'INVENTORY_VALUE', 'PROJECT_REVENUE', 'STOCK_COUNT'));

ALTER TABLE subledger_control_runs ADD COLUMN book_id VARCHAR(255);

ALTER TABLE subledger_control_runs ADD CONSTRAINT subledger_control_runs_book_for_assets
    CHECK ((subledger = 'ASSETS') = (book_id IS NOT NULL AND book_id <> '')) NOT VALID;
