-- ZS-SVC-Z-001 DQC-05 / DQC-016 CERTIFICATION_STALE (docs/architecture/DQC_GAP_MATRIX.md, step H).
--
-- A certification used to be reliable forever once issued. A contract's certification profile can now state a
-- validity window; a certification issued against it records the window and the instant it stops being
-- reliable, frozen at issue (the sealed certification is immutable, and the window it was issued under must
-- not move if the contract later changes). A certification past valid_until reads as STALE.
--
-- Both columns are NULL for a certification with no window - every certification issued before this
-- migration, and any issued against a rule set or a contract that states none. Nothing is backfilled.

ALTER TABLE dq_certifications
    ADD COLUMN IF NOT EXISTS validity_window_seconds BIGINT,
    ADD COLUMN IF NOT EXISTS valid_until             TIMESTAMPTZ;

ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_validity_together;
ALTER TABLE dq_certifications DROP CONSTRAINT IF EXISTS dq_certifications_validity_positive;
ALTER TABLE dq_certifications
    ADD CONSTRAINT dq_certifications_validity_together
        CHECK ((validity_window_seconds IS NULL) = (valid_until IS NULL)),
    ADD CONSTRAINT dq_certifications_validity_positive
        CHECK (validity_window_seconds IS NULL OR (validity_window_seconds > 0 AND valid_until > certified_at));
