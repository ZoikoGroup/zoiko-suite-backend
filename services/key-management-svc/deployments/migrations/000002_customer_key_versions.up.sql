-- key-management-svc: customer key version history, on top of 000001.
--
-- 000001 stores each key's current key_version and rotation_count only. This
-- migration records every version: rotation inserts the new version as
-- CURRENT and marks the previous one RETIRED in the same transaction, and the
-- partial unique index makes "exactly one current version" a database
-- invariant, so a concurrent or half-applied rotation cannot leave two.
--
-- WHAT THIS IS NOT: a key store. Like customer_keys, a version row records
-- that a version exists in the external KMS/HSM; no key material is held here
-- (tracker row 82 remains open).
--
-- tenant_id is UUID to match customer_keys (000001). Idempotent: test
-- harnesses re-apply every migration.

-- Target of customer_key_versions' composite FK, so a version row can never
-- belong to a different tenant than its key.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'customer_keys_tenant_id_id' AND conrelid = 'customer_keys'::regclass) THEN
        ALTER TABLE customer_keys ADD CONSTRAINT customer_keys_tenant_id_id UNIQUE (tenant_id, id);
    END IF;
    -- BYOK/HYOK keys must name the external key they refer to (mirrors
    -- RegisterKeyRequest.Validate). NOT VALID: enforced on every new or
    -- changed row without failing the migration on a pre-existing one.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'customer_keys_external_ref_required' AND conrelid = 'customer_keys'::regclass) THEN
        ALTER TABLE customer_keys ADD CONSTRAINT customer_keys_external_ref_required
            CHECK (key_model = 'SYSTEM_MANAGED' OR external_key_arn <> '') NOT VALID;
    END IF;
    -- Every rotation advances the version by exactly one.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'customer_keys_version_matches_rotations' AND conrelid = 'customer_keys'::regclass) THEN
        ALTER TABLE customer_keys ADD CONSTRAINT customer_keys_version_matches_rotations
            CHECK (key_version = rotation_count + 1) NOT VALID;
    END IF;
END$$;

CREATE TABLE IF NOT EXISTS customer_key_versions (
    tenant_id    UUID         NOT NULL,
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

-- Keys registered before this migration get their current version recorded;
-- the history of earlier rotations was never stored and is not invented.
INSERT INTO customer_key_versions (tenant_id, key_id, version, status, activated_at)
SELECT k.tenant_id, k.id, k.key_version, 'CURRENT', COALESCE(k.last_rotated_at, k.created_at)
  FROM customer_keys k
 WHERE NOT EXISTS (SELECT 1 FROM customer_key_versions v WHERE v.key_id = k.id);

ALTER TABLE customer_key_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE customer_key_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS customer_key_versions_tenant_isolation ON customer_key_versions;
CREATE POLICY customer_key_versions_tenant_isolation ON customer_key_versions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
