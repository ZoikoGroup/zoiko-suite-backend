-- Migration: 000004_add_principal_credentials.up.sql
--
-- Closes the gap that nothing on this platform authenticates a human.
--
-- Before this migration identity-context-svc could RESOLVE a principal that
-- was already authenticated (POST /v1/context/resolve verifies an inbound IdP
-- bearer token and mints the signed identity envelope) but there was no
-- credential material anywhere in the estate, so no token could ever be
-- produced in the first place. The console filled the hole with a hard-coded
-- credential list in the frontend.
--
-- principal_credentials is deliberately a SEPARATE table from principals
-- rather than two more columns on it:
--
--   1. principals is read on the hot resolve path and its rows are returned
--      to callers by GET /v1/principals/{id}. Secret material must never be
--      one forgotten column-list edit away from being serialised into a
--      response body.
--   2. Credentials have their own lifecycle (rotation, lockout, retirement)
--      that is independent of the principal's status, and a JOIN makes the
--      "principal is ACTIVE but its password is RETIRED" state expressible.
--   3. A principal of type SERVICE_ACCOUNT or API_CLIENT has no password at
--      all. A nullable column on principals would say nothing about whether
--      that is intentional; an absent row says it plainly.
--
-- secret_hash holds a PHC-encoded argon2id string
-- ($argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>) — the parameters travel
-- with each hash, so raising the cost factors later re-verifies existing
-- credentials at their original cost and lets them be re-hashed on next
-- successful use without a migration or a forced reset.
--
-- ID columns are VARCHAR, matching 000001: principal_id is a ULID, not a
-- valid Postgres UUID literal.
--
-- Migrations are run via golang-migrate CLI in CI/CD. Do NOT auto-run on
-- service startup.

CREATE TABLE principal_credentials (
    credential_id           VARCHAR(255) PRIMARY KEY,
    principal_id            VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id               VARCHAR(255) NOT NULL,

    -- Only PASSWORD is issued today. The column exists so a future
    -- WEBAUTHN/TOTP factor is an additional row rather than a second table.
    credential_type         VARCHAR(50)  NOT NULL DEFAULT 'PASSWORD',

    -- PHC-encoded argon2id digest. Never the plaintext, never a reversible
    -- encoding — this column is the reason the table is separate.
    secret_hash             TEXT         NOT NULL,

    -- Denormalised from secret_hash purely so an operator can audit which
    -- algorithm is in use estate-wide without parsing every digest.
    algorithm               VARCHAR(50)  NOT NULL DEFAULT 'argon2id',

    -- ACTIVE credentials may authenticate. RETIRED ones may not, and are kept
    -- rather than deleted so a rotation leaves a trail (doctrine §2.11).
    status                  VARCHAR(50)  NOT NULL DEFAULT 'ACTIVE',

    -- Lockout state. failed_attempt_count resets to 0 on any success.
    -- locked_until NULL means not locked; a past timestamp means the lock has
    -- expired and is treated as unlocked without needing a sweeper job.
    failed_attempt_count    INT          NOT NULL DEFAULT 0,
    locked_until            TIMESTAMP WITH TIME ZONE,

    last_authenticated_at   TIMESTAMP WITH TIME ZONE,
    secret_updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    -- One live credential of a given type per principal. Retired rows are
    -- excluded so rotation can insert the replacement before retiring the
    -- old row, rather than needing a gap where the principal has none.
    CONSTRAINT principal_credentials_status_check
        CHECK (status IN ('ACTIVE', 'RETIRED')),
    CONSTRAINT principal_credentials_attempts_check
        CHECK (failed_attempt_count >= 0)
);

CREATE UNIQUE INDEX idx_principal_credentials_one_active
    ON principal_credentials (principal_id, credential_type)
    WHERE status = 'ACTIVE';

CREATE INDEX idx_principal_credentials_tenant
    ON principal_credentials (tenant_id);

-- Authentication looks a principal up by the email the human types, so email
-- must identify exactly one principal within a tenant or the login is
-- ambiguous. 000001 constrained only (tenant_id, identity_provider_subject),
-- which is the IdP's identifier and not what anyone types into a form.
--
-- Scoped to HUMAN principals: a service account and a human may legitimately
-- share a mailbox address, and only the human authenticates with a password.
-- Lower-cased so Admin@x and admin@x cannot both exist and race.
CREATE UNIQUE INDEX idx_principals_tenant_email_human
    ON principals (tenant_id, LOWER(email))
    WHERE principal_type = 'HUMAN';

-- Same doctrine as 000003: ENABLE and FORCE from the start, WITH CHECK stated
-- explicitly rather than inherited from USING. current_setting's missing_ok
-- argument keeps an unscoped connection matching zero rows instead of raising.
--
-- This matters more here than on any other table in the service: an
-- unscoped read of principal_credentials is a read of every tenant's password
-- digests.
ALTER TABLE principal_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE principal_credentials FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON principal_credentials
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
