-- key-management-svc: first database schema.
--
-- Until this migration the service kept everything in an in-process Go map,
-- wiped on every restart. That was tolerable while it held only key METADATA
-- (tracker row 82), and is not tolerable for the token vault: a vault that
-- forgets on restart destroys every customer's bank details the moment the
-- container is rescheduled. Security Standard §12 ("platform-managed ... must
-- preserve redundancy and recovery") rules out an in-memory store for both.
-- See SPEC_DEVIATIONS.md, section 1.
--
-- Tenant IDs are UUID columns. requireTenant accepts any non-empty string,
-- but every tenant the platform issues is a UUID: tenant-entity-registry-svc's
-- tenants.tenant_id is UUID, gateway-auth-svc sets X-Tenant-Id from the signed
-- token's tenant claim, and the gateway's own mTLS platform scope is
-- 00000000-0000-0000-0000-00000000f001. The store refuses a non-UUID tenant
-- before reaching SQL rather than casting it here.

-- ── customer_keys: the existing CustomerKey model, one column per field ────
CREATE TABLE IF NOT EXISTS customer_keys (
    id               UUID        PRIMARY KEY,
    tenant_id        UUID        NOT NULL,
    legal_entity_id  TEXT        NOT NULL,
    key_alias        TEXT        NOT NULL,
    key_model        TEXT        NOT NULL
        CHECK (key_model IN ('SYSTEM_MANAGED', 'BYOK', 'HYOK')),
    key_provider     TEXT        NOT NULL
        CHECK (key_provider IN ('AWS_KMS', 'AZURE_KEY_VAULT', 'GCP_KMS', 'HASHICORP_VAULT', 'EXTERNAL_HSM')),
    external_key_arn TEXT        NOT NULL DEFAULT '',
    key_version      INTEGER     NOT NULL DEFAULT 1 CHECK (key_version >= 1),
    state            TEXT        NOT NULL
        CHECK (state IN ('ENABLED', 'DISABLED', 'PENDING_ROTATION', 'REVOKED')),
    rotation_count   INTEGER     NOT NULL DEFAULT 0 CHECK (rotation_count >= 0),
    last_rotated_at  TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_customer_keys_tenant_entity
    ON customer_keys (tenant_id, legal_entity_id);

-- ── tokenized_values: the vault ───────────────────────────────────────────
--
-- encrypted_value is AES-256-GCM ciphertext (internal/crypto), with the
-- tenant and token_id bound in as associated data, so a ciphertext copied
-- onto another row or tenant fails to decrypt instead of disclosing a value.
-- The raw value is never stored, hashed or logged anywhere in this service.
--
-- Two columns beyond the original brief, both needed by rules already in
-- this service (SPEC_DEVIATIONS.md, section 4):
--   legal_entity_id  detokenize authorizes TOKEN_DETOKENIZE against the
--                    ROW's legal entity, never the caller's claim — the same
--                    rule the key routes follow.
--   idempotency_key  tokenize is a material write (INV-08): a retried call
--                    with the same key returns the same token instead of
--                    minting an orphan copy of a bank account number.
CREATE TABLE IF NOT EXISTS tokenized_values (
    token_id                UUID        PRIMARY KEY,
    tenant_id               UUID        NOT NULL,
    legal_entity_id         TEXT        NOT NULL,
    encrypted_value         BYTEA       NOT NULL,
    classification          TEXT        NOT NULL
        CHECK (classification IN ('S2_CONFIDENTIAL', 'S3_RESTRICTED')),
    created_by_principal_id TEXT        NOT NULL,
    idempotency_key         TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT tokenized_values_idempotency UNIQUE (tenant_id, idempotency_key)
);

-- A token is a fact: once issued, what it stands for never changes. Updating
-- encrypted_value would silently repoint every stored copy of the token at a
-- different bank account. Deletion is refused too (no hard delete on material
-- records); erasure of a tokenized value is a future governed path, not a
-- DELETE (SPEC_DEVIATIONS.md, section 6).
CREATE OR REPLACE FUNCTION tokenized_values_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'tokenized_values is append-only: % refused', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tokenized_values_no_update_delete ON tokenized_values;
CREATE TRIGGER tokenized_values_no_update_delete
    BEFORE UPDATE OR DELETE ON tokenized_values
    FOR EACH ROW EXECUTE FUNCTION tokenized_values_immutable();

-- ── Row-Level Security (ENABLE + FORCE) ───────────────────────────────────
--
-- The store sets app.tenant_id per transaction (set_config(..., true)) and
-- ALSO filters on tenant_id in every query: RLS alone is not enough while any
-- environment still connects as a superuser, which bypasses it. NULLIF turns
-- an unset setting into NULL, which matches no row, rather than a cast error.
ALTER TABLE customer_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON customer_keys;
CREATE POLICY tenant_isolation_policy ON customer_keys
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE tokenized_values ENABLE ROW LEVEL SECURITY;
ALTER TABLE tokenized_values FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation_policy ON tokenized_values;
CREATE POLICY tenant_isolation_policy ON tokenized_values
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
