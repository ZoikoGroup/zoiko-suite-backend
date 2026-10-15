-- 000014 — auth plane closure (Auth-Service-audit-2026-10-14 AU-01 … AU-27).
--
-- Every table is tenant-scoped under the same RLS shape as the rest of this
-- database. Some rows are found by an unguessable secret before the tenant is
-- known (an authorization code, a refresh token, a SCIM bearer token, an
-- activation link, a federation state). Those lookups set the named
-- capability app.token_lookup, which grants SELECT only — never a write —
-- and appears in exactly one store helper (withTokenLookup). The write that
-- follows sets app.tenant_id from the row it found, like any other write.

-- ---------------------------------------------------------------------------
-- 1. Login sessions (OIDC sid) — AU-01, AU-06, AU-10
-- ---------------------------------------------------------------------------
-- One row per completed login (hosted login, federation, or the internal
-- password route). Envelopes resolved under it carry its id as sid; ending it
-- (logout, refresh-token reuse, deprovisioning, password reset) ends all of
-- them at every relying party through introspection.
CREATE TABLE IF NOT EXISTS auth_sessions (
    sid              VARCHAR(64)  PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    principal_id     VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    client_id        VARCHAR(255) NOT NULL,
    auth_time        TIMESTAMPTZ  NOT NULL,
    amr              TEXT[]       NOT NULL,
    assurance_level  VARCHAR(32)  NOT NULL,
    idp_id           VARCHAR(255),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at       TIMESTAMPTZ  NOT NULL,
    ended_at         TIMESTAMPTZ,
    end_reason       VARCHAR(64),
    client_ip_hash   VARCHAR(64),
    CONSTRAINT auth_sessions_end_pair CHECK ((ended_at IS NULL) = (end_reason IS NULL)),
    CONSTRAINT auth_sessions_assurance CHECK (assurance_level IN ('SINGLE_FACTOR', 'MFA', 'PHISHING_RESISTANT'))
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_principal_live
    ON auth_sessions (tenant_id, principal_id) WHERE ended_at IS NULL;

ALTER TABLE session_contexts ADD COLUMN IF NOT EXISTS auth_session_id VARCHAR(64);
CREATE INDEX IF NOT EXISTS idx_session_contexts_auth_session
    ON session_contexts (auth_session_id) WHERE auth_session_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. OAuth artefacts — AU-06, AU-10, AU-27
-- ---------------------------------------------------------------------------
-- Codes and refresh tokens are stored as SHA-256 digests: the database never
-- holds a usable credential.
CREATE TABLE IF NOT EXISTS oauth_authorization_codes (
    code_hash              CHAR(64)     PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    client_id              VARCHAR(255) NOT NULL,
    redirect_uri           TEXT         NOT NULL,
    sid                    VARCHAR(64)  NOT NULL REFERENCES auth_sessions(sid),
    principal_id           VARCHAR(255) NOT NULL,
    scope                  TEXT         NOT NULL,
    nonce                  TEXT,
    code_challenge         VARCHAR(128) NOT NULL,
    code_challenge_method  VARCHAR(8)   NOT NULL CHECK (code_challenge_method = 'S256'),
    dpop_jkt               VARCHAR(64),
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at             TIMESTAMPTZ  NOT NULL,
    consumed_at            TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
    token_hash         CHAR(64)     PRIMARY KEY,
    family_id          VARCHAR(64)  NOT NULL,
    tenant_id          VARCHAR(255) NOT NULL,
    principal_id       VARCHAR(255) NOT NULL,
    client_id          VARCHAR(255) NOT NULL,
    sid                VARCHAR(64)  NOT NULL REFERENCES auth_sessions(sid),
    scope              TEXT         NOT NULL,
    dpop_jkt           VARCHAR(64),
    issued_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ  NOT NULL,
    family_expires_at  TIMESTAMPTZ  NOT NULL,
    used_at            TIMESTAMPTZ,
    revoked_at         TIMESTAMPTZ,
    revoke_reason      VARCHAR(64)
);
CREATE INDEX IF NOT EXISTS idx_refresh_family ON oauth_refresh_tokens (family_id);
CREATE INDEX IF NOT EXISTS idx_refresh_sid ON oauth_refresh_tokens (sid);

-- Access tokens revoked before expiry (RFC 7009). Kept until they expire.
CREATE TABLE IF NOT EXISTS oauth_revoked_tokens (
    jti         VARCHAR(64)  PRIMARY KEY,
    tenant_id   VARCHAR(255) NOT NULL,
    expires_at  TIMESTAMPTZ  NOT NULL,
    revoked_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- 3. Subject lifecycle — AU-09
-- ---------------------------------------------------------------------------
ALTER TABLE principals
    ADD COLUMN IF NOT EXISTS external_id      VARCHAR(255),
    ADD COLUMN IF NOT EXISTS source           VARCHAR(32) NOT NULL DEFAULT 'LOCAL',
    ADD COLUMN IF NOT EXISTS created_by       VARCHAR(255),
    ADD COLUMN IF NOT EXISTS invited_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS activated_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS disabled_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS disabled_reason  VARCHAR(100);
CREATE UNIQUE INDEX IF NOT EXISTS idx_principals_tenant_external_id
    ON principals (tenant_id, external_id) WHERE external_id IS NOT NULL;

-- One-time credential links: invitation/activation and password reset.
CREATE TABLE IF NOT EXISTS credential_tokens (
    token_hash    CHAR(64)     PRIMARY KEY,
    tenant_id     VARCHAR(255) NOT NULL,
    principal_id  VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    purpose       VARCHAR(16)  NOT NULL CHECK (purpose IN ('ACTIVATION', 'RESET')),
    request_id    VARCHAR(64),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ  NOT NULL,
    consumed_at   TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_credential_tokens_principal ON credential_tokens (tenant_id, principal_id);

-- Administrator-initiated password resets are independently approved before
-- a link is issued (IAM §3 "credential recovery is separately controlled").
CREATE TABLE IF NOT EXISTS password_reset_requests (
    request_id          VARCHAR(64)  PRIMARY KEY,
    tenant_id           VARCHAR(255) NOT NULL,
    principal_id        VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    requested_by        VARCHAR(255) NOT NULL,
    reason              TEXT         NOT NULL,
    approvals_required  INT          NOT NULL CHECK (approvals_required >= 1),
    approvals           JSONB        NOT NULL DEFAULT '[]'::jsonb,
    status              VARCHAR(24)  NOT NULL CHECK (status IN ('PENDING_APPROVAL', 'ISSUED', 'REJECTED', 'COMPLETED', 'EXPIRED')),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    decided_at          TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ  NOT NULL,
    CONSTRAINT password_reset_not_self CHECK (requested_by <> principal_id)
);

CREATE TABLE IF NOT EXISTS password_history (
    principal_id  VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id     VARCHAR(255) NOT NULL,
    secret_hash   TEXT         NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_password_history_principal ON password_history (tenant_id, principal_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- 4. Federation — AU-07
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS identity_providers (
    idp_id                 VARCHAR(64)  PRIMARY KEY,
    tenant_id              VARCHAR(255) NOT NULL,
    protocol               VARCHAR(8)   NOT NULL CHECK (protocol IN ('OIDC', 'SAML')),
    display_name           VARCHAR(255) NOT NULL,
    -- OIDC
    issuer                 TEXT,
    client_id              VARCHAR(255),
    client_secret_ref      VARCHAR(255),
    scopes                 TEXT,
    -- SAML
    saml_idp_entity_id     TEXT,
    saml_sso_url           TEXT,
    saml_cert_pem          TEXT,
    -- Claim mapping and provisioning
    claim_email            VARCHAR(64)  NOT NULL DEFAULT 'email',
    claim_name             VARCHAR(64)  NOT NULL DEFAULT 'name',
    claim_groups           VARCHAR(64),
    jit_provisioning       BOOLEAN      NOT NULL DEFAULT FALSE,
    link_by_email          BOOLEAN      NOT NULL DEFAULT FALSE,
    allowed_domains        TEXT[]       NOT NULL DEFAULT '{}',
    status                 VARCHAR(16)  NOT NULL CHECK (status IN ('ACTIVE', 'DISABLED')),
    version                INT          NOT NULL DEFAULT 1,
    created_by             VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT identity_providers_oidc_shape CHECK (protocol <> 'OIDC' OR (issuer IS NOT NULL AND client_id IS NOT NULL)),
    CONSTRAINT identity_providers_saml_shape CHECK (protocol <> 'SAML' OR (saml_idp_entity_id IS NOT NULL AND saml_sso_url IS NOT NULL AND saml_cert_pem IS NOT NULL))
);

CREATE TABLE IF NOT EXISTS federated_identities (
    idp_id            VARCHAR(64)  NOT NULL REFERENCES identity_providers(idp_id),
    external_subject  VARCHAR(512) NOT NULL,
    tenant_id         VARCHAR(255) NOT NULL,
    principal_id      VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    email             VARCHAR(320),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_login_at     TIMESTAMPTZ,
    PRIMARY KEY (idp_id, external_subject)
);
CREATE INDEX IF NOT EXISTS idx_federated_identities_principal ON federated_identities (tenant_id, principal_id);

-- ---------------------------------------------------------------------------
-- 5. SCIM provisioning — AU-08
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS scim_connections (
    connection_id  VARCHAR(64)  PRIMARY KEY,
    tenant_id      VARCHAR(255) NOT NULL,
    idp_id         VARCHAR(64),
    token_hash     CHAR(64)     NOT NULL UNIQUE,
    status         VARCHAR(16)  NOT NULL CHECK (status IN ('ACTIVE', 'REVOKED')),
    created_by     VARCHAR(255) NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_sync_at   TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS identity_groups (
    group_id      VARCHAR(64)  PRIMARY KEY,
    tenant_id     VARCHAR(255) NOT NULL,
    display_name  VARCHAR(255) NOT NULL,
    external_id   VARCHAR(255),
    source        VARCHAR(32)  NOT NULL DEFAULT 'SCIM',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_identity_groups_name
    ON identity_groups (tenant_id, LOWER(display_name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS group_memberships (
    group_id      VARCHAR(64)  NOT NULL REFERENCES identity_groups(group_id),
    principal_id  VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id     VARCHAR(255) NOT NULL,
    source        VARCHAR(32)  NOT NULL DEFAULT 'SCIM',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (group_id, principal_id)
);

-- Group-to-role mappings are tenant-owned, versioned and independently
-- approved before they take effect (IAM §17; A20). A membership in a group
-- with an ACTIVE mapping becomes an access REQUEST at access-control-svc,
-- never a direct grant, so a protected role still takes its approval path.
CREATE TABLE IF NOT EXISTS group_role_mappings (
    mapping_id       VARCHAR(64)  PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL,
    group_id         VARCHAR(64)  NOT NULL REFERENCES identity_groups(group_id),
    role_id          VARCHAR(255) NOT NULL,
    legal_entity_id  VARCHAR(255),
    status           VARCHAR(16)  NOT NULL CHECK (status IN ('PROPOSED', 'ACTIVE', 'REJECTED', 'RETIRED')),
    version          INT          NOT NULL DEFAULT 1,
    reason           TEXT         NOT NULL,
    proposed_by      VARCHAR(255) NOT NULL,
    proposed_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    decided_by       VARCHAR(255),
    decided_at       TIMESTAMPTZ,
    retired_by       VARCHAR(255),
    retired_at       TIMESTAMPTZ,
    CONSTRAINT group_role_mapping_maker_checker CHECK (decided_by IS NULL OR decided_by <> proposed_by)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_group_role_mappings_live
    ON group_role_mappings (tenant_id, group_id, role_id, COALESCE(legal_entity_id, '')) WHERE status IN ('PROPOSED', 'ACTIVE');

CREATE TABLE IF NOT EXISTS group_role_grants (
    mapping_id         VARCHAR(64)  NOT NULL REFERENCES group_role_mappings(mapping_id),
    principal_id       VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id          VARCHAR(255) NOT NULL,
    access_request_id  VARCHAR(255),
    status             VARCHAR(16)  NOT NULL CHECK (status IN ('REQUESTED', 'REVOKE_REQUESTED', 'FAILED')),
    detail             TEXT,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (mapping_id, principal_id)
);

-- ---------------------------------------------------------------------------
-- 6. WebAuthn / passkeys — AU-11
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webauthn_credentials (
    credential_id     TEXT         PRIMARY KEY,
    tenant_id         VARCHAR(255) NOT NULL,
    principal_id      VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    public_key        BYTEA        NOT NULL,
    attestation_type  VARCHAR(32)  NOT NULL,
    aaguid            VARCHAR(64),
    sign_count        BIGINT       NOT NULL DEFAULT 0,
    transports        TEXT[]       NOT NULL DEFAULT '{}',
    backup_eligible   BOOLEAN      NOT NULL DEFAULT FALSE,
    backup_state      BOOLEAN      NOT NULL DEFAULT FALSE,
    user_verified     BOOLEAN      NOT NULL DEFAULT FALSE,
    nickname          VARCHAR(100),
    status            VARCHAR(16)  NOT NULL CHECK (status IN ('ACTIVE', 'RETIRED')),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_used_at      TIMESTAMPTZ,
    retired_at        TIMESTAMPTZ,
    retired_reason    VARCHAR(100)
);
CREATE INDEX IF NOT EXISTS idx_webauthn_principal ON webauthn_credentials (tenant_id, principal_id) WHERE status = 'ACTIVE';

-- ---------------------------------------------------------------------------
-- 7. Row-level security
-- ---------------------------------------------------------------------------
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'auth_sessions', 'oauth_authorization_codes', 'oauth_refresh_tokens', 'oauth_revoked_tokens',
        'credential_tokens', 'password_reset_requests', 'password_history',
        'identity_providers', 'federated_identities', 'scim_connections',
        'identity_groups', 'group_memberships', 'group_role_mappings', 'group_role_grants',
        'webauthn_credentials'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = current_setting('app.tenant_id', true))
            WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$p$, t);
    END LOOP;
    -- The token-lookup hatch: SELECT only, on the tables a secret or an
    -- identifier from outside a tenant leads to.
    FOREACH t IN ARRAY ARRAY[
        'auth_sessions', 'oauth_authorization_codes', 'oauth_refresh_tokens', 'oauth_revoked_tokens',
        'credential_tokens', 'identity_providers', 'federated_identities', 'scim_connections',
        'webauthn_credentials'
    ] LOOP
        EXECUTE format('DROP POLICY IF EXISTS token_lookup_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY token_lookup_policy ON %I FOR SELECT
            USING (current_setting('app.token_lookup', true) = 'true')$p$, t);
    END LOOP;
END $$;

-- Expired codes, revoked-jti rows, spent refresh families and links are
-- purged by the retention worker under its existing named capability.
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['oauth_authorization_codes', 'oauth_revoked_tokens', 'oauth_refresh_tokens', 'credential_tokens'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS retention_purge_policy ON %I', t);
        -- FOR ALL so the sweep can read the rows it deletes; WITH CHECK
        -- (false) so the capability can never write or move a row.
        EXECUTE format($p$CREATE POLICY retention_purge_policy ON %I FOR ALL
            USING (current_setting('app.retention_sweep', true) = 'true')
            WITH CHECK (false)$p$, t);
    END LOOP;
END $$;

-- session_contexts: introspection reads a session by id with the tenant from
-- a verified envelope, so it needs no hatch. principals: a SCIM or federation
-- lookup is always tenant-scoped once the connection or IdP row is found.

-- Grants for the application role, guarded (create-app-roles.sh also grants
-- tables on a fresh volume, after migrations).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_identity_context') THEN
        EXECUTE 'GRANT SELECT, INSERT, UPDATE, DELETE ON
            auth_sessions, oauth_authorization_codes, oauth_refresh_tokens, oauth_revoked_tokens,
            credential_tokens, password_reset_requests, password_history,
            identity_providers, federated_identities, scim_connections,
            identity_groups, group_memberships, group_role_mappings, group_role_grants,
            webauthn_credentials TO app_identity_context';
    END IF;
END $$;
