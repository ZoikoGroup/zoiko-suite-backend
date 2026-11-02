-- key-management-svc: customer key METADATA registry and version lifecycle.
--
-- WHAT THIS IS NOT: a key store. No key material — plaintext, wrapped or
-- otherwise — is held in this database or anywhere in this service. A row
-- records that a key exists in an external KMS/HSM (external_key_arn), who it
-- belongs to (tenant, legal entity), its custody model (SYSTEM_MANAGED / BYOK /
-- HYOK) and its lifecycle (state, version, rotation history). Tracker row 82
-- ("never actually used to encrypt/decrypt anything") remains open: nothing in
-- the platform calls this service to perform a cryptographic operation.
--
-- Authoritative state (no cache): every column below. Until this migration it
-- lived in process memory and vanished on restart (tracker rows 8k/90 and the
-- "no Postgres at all" note).
--
-- Fields follow the subset of the Cryptographic Architecture Standard's
-- KeyRecord (§11.1: key_id, provider, external_key_id, custody_mode, status,
-- version, created_at, activated_at, retired_at) that the service's existing
-- API already models; the rest of KeyRecord is not invented here.

CREATE TABLE IF NOT EXISTS customer_keys (
    id               UUID         PRIMARY KEY,
    tenant_id        VARCHAR(255) NOT NULL CHECK (tenant_id <> ''),
    legal_entity_id  VARCHAR(255) NOT NULL CHECK (legal_entity_id <> ''),
    key_alias        VARCHAR(255) NOT NULL CHECK (key_alias <> ''),
    key_model        VARCHAR(32)  NOT NULL CHECK (key_model IN ('SYSTEM_MANAGED', 'BYOK', 'HYOK')),
    key_provider     VARCHAR(32)  NOT NULL CHECK (key_provider IN ('AWS_KMS', 'AZURE_KEY_VAULT', 'GCP_KMS', 'HASHICORP_VAULT', 'EXTERNAL_HSM')),
    -- A REFERENCE to the key in the external KMS, never the key itself.
    external_key_arn TEXT         NOT NULL DEFAULT '',
    key_version      INT          NOT NULL DEFAULT 1 CHECK (key_version >= 1),
    state            VARCHAR(32)  NOT NULL CHECK (state IN ('ENABLED', 'DISABLED', 'PENDING_ROTATION', 'REVOKED')),
    rotation_count   INT          NOT NULL DEFAULT 0 CHECK (rotation_count >= 0),
    last_rotated_at  TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    -- BYOK/HYOK keys must name the external key they refer to (mirrors
    -- RegisterKeyRequest.Validate).
    CONSTRAINT customer_keys_external_ref_required
        CHECK (key_model = 'SYSTEM_MANAGED' OR external_key_arn <> ''),
    -- Every rotation advances the version by exactly one.
    CONSTRAINT customer_keys_version_matches_rotations
        CHECK (key_version = rotation_count + 1),
    -- Target of customer_key_versions' composite FK, so a version row can
    -- never belong to a different tenant than its key.
    CONSTRAINT customer_keys_tenant_id_id UNIQUE (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS idx_customer_keys_scope
    ON customer_keys (tenant_id, legal_entity_id, created_at);

-- One row per version of a key. Rotation inserts the new version as CURRENT
-- and marks the previous one RETIRED in the same transaction; the partial
-- unique index makes "exactly one current version" a database invariant, so a
-- concurrent or half-applied rotation cannot leave two.
CREATE TABLE IF NOT EXISTS customer_key_versions (
    tenant_id    VARCHAR(255) NOT NULL CHECK (tenant_id <> ''),
    key_id       UUID         NOT NULL,
    version      INT          NOT NULL CHECK (version >= 1),
    status       VARCHAR(16)  NOT NULL CHECK (status IN ('CURRENT', 'RETIRED')),
    activated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    retired_at   TIMESTAMPTZ,
    PRIMARY KEY (key_id, version),
    CONSTRAINT customer_key_versions_key_fk
        FOREIGN KEY (tenant_id, key_id) REFERENCES customer_keys (tenant_id, id) ON DELETE RESTRICT,
    CONSTRAINT customer_key_versions_retired_at
        CHECK ((status = 'RETIRED') = (retired_at IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_customer_key_versions_one_current
    ON customer_key_versions (key_id) WHERE status = 'CURRENT';

ALTER TABLE customer_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS customer_keys_tenant_isolation ON customer_keys;
CREATE POLICY customer_keys_tenant_isolation ON customer_keys FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE customer_key_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_key_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS customer_key_versions_tenant_isolation ON customer_key_versions;
CREATE POLICY customer_key_versions_tenant_isolation ON customer_key_versions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
