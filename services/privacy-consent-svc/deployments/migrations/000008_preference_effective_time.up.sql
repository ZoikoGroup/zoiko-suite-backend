-- 000008_preference_effective_time.up.sql
-- §10.1: PreferenceAssertion carries "source and effective time". effective_at
-- is that instant, separate from created_at (when it was recorded).
--
-- preference_assertions is append-only evidence (000002 rejects UPDATE), so
-- existing rows are NOT rewritten: the column is nullable and a NULL means
-- "effective from created_at" (readers use COALESCE(effective_at, created_at)).
-- Every row written from now on stores an explicit value.
ALTER TABLE preference_assertions ADD COLUMN IF NOT EXISTS effective_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_preference_assertions_effective
    ON preference_assertions (subject_ref, channel_or_purpose, (COALESCE(effective_at, created_at)) DESC);
