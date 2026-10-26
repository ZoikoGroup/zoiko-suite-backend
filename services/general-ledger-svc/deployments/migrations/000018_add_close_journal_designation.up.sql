-- Migration: 000018_add_close_journal_designation.up.sql
--
-- ZS-SVC-B-001 ACC-14 §10.1 gives CLOSE_REVIEW the posting policy
-- "Only designated close journals/workflows permitted". The specification
-- names the category but does not define a designation mechanism, so
-- general-ledger-svc persists the narrowest one that is still a property of
-- the journal rather than a permission-only bypass:
--
--   close_journal_designation names the close workflow that produced the
--   journal. It is a closed allowlist (ACC-07 accrual recognition, ACC-08
--   prepayment recognition, ACC-08 prepayment termination), and every value
--   must agree with the journal's ACC-03 type (all three are ACCRUAL).
--
-- The column is nullable: ordinary journals have no designation and are still
-- refused in CLOSE_REVIEW. The database CHECK is a second line of defence
-- behind the handler's validation; it does not replace the handler's
-- GL_CLOSE_REVIEW_POSTING authorization check, which decides whether a
-- particular caller may use a valid designation.
--
-- This column is also the new field a future database-level HardClosed guard
-- (financial-close-svc SPEC_DEVIATIONS A5) must consult: a DB guard that only
-- knows the period state and not the designation would either block a
-- legitimate designated close journal or be too permissive. See
-- general-ledger-svc SPEC_DEVIATIONS A14.

ALTER TABLE journal_headers
    ADD COLUMN close_journal_designation VARCHAR(64);

ALTER TABLE journal_headers
    ADD CONSTRAINT journal_headers_close_journal_designation_check
    CHECK (
        close_journal_designation IS NULL
        OR (
            close_journal_designation IN (
                'ACCRUAL_RECOGNITION',
                'PREPAYMENT_RECOGNITION',
                'PREPAYMENT_TERMINATION'
            )
            AND journal_type = 'ACCRUAL'
        )
    );
