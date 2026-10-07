-- 000006_pack_artifacts_and_signing_keys.up.sql
-- ZS-JUR-001 Wave 1: compiled pack artifacts, their signatures, and the
-- registry of trusted signing public keys. Additive; 000005 is untouched.
--
-- Private keys are NEVER stored here (s28: credentials and keys live in the
-- secrets platform). Only public keys are registered, so a runtime loader can
-- verify an artifact without being able to forge one.

-- ── trusted signing keys ────────────────────────────────────────────────────
CREATE TABLE pack_signing_keys (
    key_ref                 VARCHAR(128) PRIMARY KEY,
    algorithm               VARCHAR(32)  NOT NULL DEFAULT 'ED25519',
    public_key              BYTEA        NOT NULL,
    -- ACTIVE: may sign and verify. RETIRED: rotated out, may still VERIFY
    -- what it already signed (historical reconstruction). REVOKED:
    -- compromised, verification fails.
    status                  VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE',
    status_reason           TEXT,
    status_changed_at       TIMESTAMPTZ,
    status_changed_by       TEXT,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id TEXT         NOT NULL,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT ck_pack_signing_key_ed25519 CHECK (algorithm <> 'ED25519' OR octet_length(public_key) = 32),
    CONSTRAINT ck_pack_signing_key_status CHECK (status IN ('ACTIVE', 'RETIRED', 'REVOKED')),
    CONSTRAINT ck_pack_signing_key_ref_shape CHECK (key_ref ~ '^[a-z0-9][a-z0-9._:-]*$')
);

-- A key's identity and material never change; status only moves forward
-- (ACTIVE -> RETIRED -> REVOKED, or ACTIVE -> REVOKED); never deleted.
CREATE FUNCTION jur_signing_key_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.key_ref <> OLD.key_ref OR NEW.algorithm <> OLD.algorithm OR NEW.public_key <> OLD.public_key
       OR NEW.created_at <> OLD.created_at OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'signing key % identity and material are immutable', OLD.key_ref USING ERRCODE = '23514';
    END IF;
    IF NEW.status <> OLD.status AND NOT ((OLD.status, NEW.status) IN
        (('ACTIVE','RETIRED'), ('ACTIVE','REVOKED'), ('RETIRED','REVOKED'))) THEN
        RAISE EXCEPTION 'illegal signing key status transition % -> %', OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_signing_key_guard BEFORE UPDATE ON pack_signing_keys
    FOR EACH ROW EXECUTE FUNCTION jur_signing_key_guard();
CREATE TRIGGER trg_pack_signing_key_no_delete BEFORE DELETE ON pack_signing_keys
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();

-- ── compiled artifacts ──────────────────────────────────────────────────────
CREATE TABLE pack_artifacts (
    pack_version_id         UUID         PRIMARY KEY REFERENCES jurisdiction_pack_versions(pack_version_id),
    -- The canonical JSON bytes that were hashed. Stored as text, not JSONB,
    -- so the digest can always be recomputed from exactly what was signed.
    artifact                TEXT         NOT NULL,
    artifact_digest         VARCHAR(71)  NOT NULL,
    compiler_name           VARCHAR(64)  NOT NULL,
    compiler_version        VARCHAR(32)  NOT NULL,
    -- Diagnostics from the compile (warnings, pinned-but-unverified
    -- dependencies). Not part of the digest.
    report                  JSONB        NOT NULL,
    compiled_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    compiled_by_principal_id TEXT        NOT NULL,
    signature               TEXT,
    signature_key_ref       VARCHAR(128) REFERENCES pack_signing_keys(key_ref),
    signed_at               TIMESTAMPTZ,
    signed_by_principal_id  TEXT,
    schema_version          VARCHAR(16)  NOT NULL DEFAULT '1.0',
    CONSTRAINT ck_pack_artifact_digest_shape CHECK (artifact_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT ck_pack_artifact_signature_complete CHECK (
        (signature IS NULL AND signature_key_ref IS NULL AND signed_at IS NULL AND signed_by_principal_id IS NULL)
        OR (signature IS NOT NULL AND signature_key_ref IS NOT NULL AND signed_at IS NOT NULL AND signed_by_principal_id IS NOT NULL))
);
CREATE UNIQUE INDEX uq_pack_artifacts_digest_per_version ON pack_artifacts (artifact_digest, pack_version_id);

-- The artifact is the released representation (s3): its bytes and digest never
-- change, and the signature is written exactly once. It can only be created
-- while the version is under REVIEW.
CREATE FUNCTION jur_pack_artifact_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (SELECT status FROM jurisdiction_pack_versions WHERE pack_version_id = NEW.pack_version_id) <> 'REVIEW' THEN
            RAISE EXCEPTION 'an artifact can only be compiled for a version under REVIEW' USING ERRCODE = '23514';
        END IF;
        IF NEW.signature IS NOT NULL THEN
            RAISE EXCEPTION 'an artifact is created unsigned; signing is a separate step' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.pack_version_id <> OLD.pack_version_id OR NEW.artifact <> OLD.artifact OR NEW.artifact_digest <> OLD.artifact_digest
       OR NEW.compiler_name <> OLD.compiler_name OR NEW.compiler_version <> OLD.compiler_version
       OR NEW.report::text <> OLD.report::text OR NEW.compiled_at <> OLD.compiled_at
       OR NEW.compiled_by_principal_id <> OLD.compiled_by_principal_id THEN
        RAISE EXCEPTION 'a compiled pack artifact is immutable; compile a new version' USING ERRCODE = '23514';
    END IF;
    IF OLD.signature IS NOT NULL AND (NEW.signature IS DISTINCT FROM OLD.signature
       OR NEW.signature_key_ref IS DISTINCT FROM OLD.signature_key_ref
       OR NEW.signed_at IS DISTINCT FROM OLD.signed_at
       OR NEW.signed_by_principal_id IS DISTINCT FROM OLD.signed_by_principal_id) THEN
        RAISE EXCEPTION 'artifact signature is write-once' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER trg_pack_artifact_guard BEFORE INSERT OR UPDATE ON pack_artifacts
    FOR EACH ROW EXECUTE FUNCTION jur_pack_artifact_guard();
CREATE TRIGGER trg_pack_artifact_no_delete BEFORE DELETE ON pack_artifacts
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
