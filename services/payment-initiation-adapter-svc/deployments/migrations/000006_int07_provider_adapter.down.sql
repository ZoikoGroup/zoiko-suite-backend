DROP POLICY IF EXISTS attempts_sweeper_read ON payment_initiation_attempts;
DROP TABLE IF EXISTS provider_transport_log;
DROP TABLE IF EXISTS provider_profiles;
DROP FUNCTION IF EXISTS provider_profile_guard();
ALTER TABLE payment_initiation_attempts DROP CONSTRAINT IF EXISTS attempt_in_flight_consistent;
DROP INDEX IF EXISTS idx_attempts_in_flight;
UPDATE payment_initiation_attempts SET status = 'PENDING_UNKNOWN', in_flight_since = NULL WHERE status = 'SUBMITTING';
ALTER TABLE payment_initiation_attempts DROP CONSTRAINT IF EXISTS payment_initiation_attempts_status_check;
ALTER TABLE payment_initiation_attempts ADD CONSTRAINT payment_initiation_attempts_status_check
    CHECK (status IN ('PREPARED', 'SUBMITTED', 'PENDING_UNKNOWN', 'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED'));
ALTER TABLE payment_initiation_attempts
    DROP COLUMN IF EXISTS provider_key, DROP COLUMN IF EXISTS profile_version, DROP COLUMN IF EXISTS rail,
    DROP COLUMN IF EXISTS connector_id, DROP COLUMN IF EXISTS sent_payload_hash, DROP COLUMN IF EXISTS sent_at,
    DROP COLUMN IF EXISTS in_flight_since, DROP COLUMN IF EXISTS provider_status, DROP COLUMN IF EXISTS send_count,
    DROP COLUMN IF EXISTS not_before, DROP COLUMN IF EXISTS last_failure;
