DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS authorization_signatures;

DROP TRIGGER IF EXISTS trg_bump_authorization_version ON payment_authorizations;
DROP FUNCTION IF EXISTS bump_authorization_version();
DROP INDEX IF EXISTS idx_payment_authorizations_expiry;

ALTER TABLE payment_authorizations
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS signature_count,
    DROP COLUMN IF EXISTS required_signatures,
    DROP COLUMN IF EXISTS version;
