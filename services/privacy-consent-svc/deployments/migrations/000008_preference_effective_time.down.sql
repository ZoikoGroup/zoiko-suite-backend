-- 000008_preference_effective_time.down.sql
DROP INDEX IF EXISTS idx_preference_assertions_effective;
ALTER TABLE preference_assertions DROP COLUMN IF EXISTS effective_at;
