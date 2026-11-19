-- Migration 000012: the TOTP second factor (ZS-IAM-001 §3 "no password-only
-- privileged sessions", §13 / §14 step-up before a JIT or break-glass session;
-- authorization-svc register S-3 / A-03, Group 1 register R-2).
--
-- 000004 created principal_credentials so that "a future WEBAUTHN/TOTP factor
-- is an additional row rather than a second table". This is that row. What
-- differs from a password:
--
--   * The secret cannot be hashed: the server needs it to compute the expected
--     code. It is AES-256-GCM encrypted under MFA_SECRET_KEY (held outside the
--     database) in secret_ciphertext, with the sealing key's id beside it, and
--     secret_hash stays NULL. The shape CHECK below makes the two kinds of
--     secret mutually exclusive by type, so a TOTP row can never be read as a
--     password digest or the reverse.
--   * A factor is PENDING from generation until a code from it verifies. A
--     PENDING row grants nothing.
--   * totp_last_used_step records the last accepted RFC 6238 time step. A code
--     is valid for its whole 30 s window (90 s with the ±1 drift allowance), so
--     without it an observed code could be replayed.
--
-- failed_attempt_count / locked_until are reused unchanged: a TOTP factor has
-- its own lockout, independent of the password's.

ALTER TABLE principal_credentials
    ADD COLUMN IF NOT EXISTS secret_ciphertext       TEXT,
    ADD COLUMN IF NOT EXISTS secret_key_id           VARCHAR(64),
    ADD COLUMN IF NOT EXISTS totp_last_used_step     BIGINT,
    ADD COLUMN IF NOT EXISTS activated_at            TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS retired_at              TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS retired_reason          VARCHAR(100),
    ADD COLUMN IF NOT EXISTS retired_by_principal_id VARCHAR(255);

ALTER TABLE principal_credentials ALTER COLUMN secret_hash DROP NOT NULL;

ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_status_check;
ALTER TABLE principal_credentials ADD CONSTRAINT principal_credentials_status_check
    CHECK (status IN ('ACTIVE', 'RETIRED', 'PENDING'));

ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_type_check;
ALTER TABLE principal_credentials ADD CONSTRAINT principal_credentials_type_check
    CHECK (credential_type IN ('PASSWORD', 'TOTP'));

-- Only a factor that must be proven before use has a PENDING state.
ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_pending_totp_check;
ALTER TABLE principal_credentials ADD CONSTRAINT principal_credentials_pending_totp_check
    CHECK (status <> 'PENDING' OR credential_type = 'TOTP');

ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_secret_shape_check;
ALTER TABLE principal_credentials ADD CONSTRAINT principal_credentials_secret_shape_check
    CHECK (
        (credential_type = 'PASSWORD' AND secret_hash IS NOT NULL AND secret_ciphertext IS NULL)
     OR (credential_type = 'TOTP' AND secret_hash IS NULL
         AND secret_ciphertext IS NOT NULL AND secret_key_id IS NOT NULL)
    );

-- An ACTIVE TOTP factor was proven: it has an activation time.
ALTER TABLE principal_credentials DROP CONSTRAINT IF EXISTS principal_credentials_totp_activated_check;
ALTER TABLE principal_credentials ADD CONSTRAINT principal_credentials_totp_activated_check
    CHECK (credential_type <> 'TOTP' OR status <> 'ACTIVE' OR activated_at IS NOT NULL);

-- One enrolment in flight per principal; a new enrolment retires the old one.
-- (One ACTIVE per type is already idx_principal_credentials_one_active.)
CREATE UNIQUE INDEX IF NOT EXISTS idx_principal_credentials_one_pending
    ON principal_credentials (principal_id, credential_type)
    WHERE status = 'PENDING';
