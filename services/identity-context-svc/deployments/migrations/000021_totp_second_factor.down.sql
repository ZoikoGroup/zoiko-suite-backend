-- Reverts 000012. Refuses while any TOTP factor exists: dropping the columns
-- would silently delete every enrolled second factor, and a principal relying
-- on one for privileged sessions would lose it with no record. Retire and
-- delete the factors deliberately first if a rollback is really meant.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM principal_credentials WHERE credential_type = 'TOTP') THEN
        RAISE EXCEPTION '000012 down refused: TOTP factors exist in principal_credentials';
    END IF;
END $$;

DROP INDEX IF EXISTS idx_principal_credentials_one_pending;

ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_totp_activated_check;
ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_secret_shape_check;
ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_pending_totp_check;
ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_type_check;
ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_status_check;
ALTER TABLE principal_credentials ADD CONSTRAINT principal_credentials_status_check
    CHECK (status IN ('ACTIVE', 'RETIRED'));

ALTER TABLE principal_credentials ALTER COLUMN secret_hash SET NOT NULL;

ALTER TABLE principal_credentials
    DROP COLUMN IF EXISTS retired_by_principal_id,
    DROP COLUMN IF EXISTS retired_reason,
    DROP COLUMN IF EXISTS retired_at,
    DROP COLUMN IF EXISTS activated_at,
    DROP COLUMN IF EXISTS totp_last_used_step,
    DROP COLUMN IF EXISTS secret_key_id,
    DROP COLUMN IF EXISTS secret_ciphertext;
