-- Reverses 000014. Drops credentials (refresh tokens, links, passkeys) and
-- federation/SCIM configuration; run only to roll back the release.
DROP TABLE IF EXISTS webauthn_credentials;
DROP TABLE IF EXISTS group_role_grants;
DROP TABLE IF EXISTS group_role_mappings;
DROP TABLE IF EXISTS group_memberships;
DROP TABLE IF EXISTS identity_groups;
DROP TABLE IF EXISTS scim_connections;
DROP TABLE IF EXISTS federated_identities;
DROP TABLE IF EXISTS identity_providers;
DROP TABLE IF EXISTS password_history;
DROP TABLE IF EXISTS password_reset_requests;
DROP TABLE IF EXISTS credential_tokens;
DROP TABLE IF EXISTS oauth_revoked_tokens;
DROP TABLE IF EXISTS oauth_refresh_tokens;
DROP TABLE IF EXISTS oauth_authorization_codes;
DROP INDEX IF EXISTS idx_session_contexts_auth_session;
ALTER TABLE session_contexts DROP COLUMN IF EXISTS auth_session_id;
DROP TABLE IF EXISTS auth_sessions;
DROP INDEX IF EXISTS idx_principals_tenant_external_id;
ALTER TABLE principals
    DROP COLUMN IF EXISTS external_id,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS invited_at,
    DROP COLUMN IF EXISTS activated_at,
    DROP COLUMN IF EXISTS disabled_at,
    DROP COLUMN IF EXISTS disabled_reason;
