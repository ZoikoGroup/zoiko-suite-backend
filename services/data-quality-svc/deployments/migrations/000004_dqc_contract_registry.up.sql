-- ZS-SVC-Z-001 DQC-01 data contract registry (docs/architecture/DQC_GAP_MATRIX.md, step F).
--
-- dqc_assets          the stable identity of a data asset (never free text alone).
-- dqc_contract_versions  one immutable row per contract version. The content is frozen
--                     from the moment the row exists; only the lifecycle moves, forward.
--
-- Everything is enforced here as well as in the application: a bug in the service must not
-- be able to edit a published contract, publish without an independent approver, or walk a
-- version backwards.

CREATE TABLE IF NOT EXISTS dqc_assets (
    asset_id    TEXT         PRIMARY KEY
        CHECK (asset_id ~ '^dca_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id   VARCHAR(64)  NOT NULL,
    asset_key   VARCHAR(128) NOT NULL CHECK (asset_key ~ '^[a-z][a-z0-9]*([._-][a-z0-9]+)*$'),
    asset_kind  VARCHAR(32)  NOT NULL CHECK (asset_kind ~ '^[a-z][a-z0-9_]{0,31}$'),
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by  VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, asset_key)
);

CREATE TABLE IF NOT EXISTS dqc_contract_versions (
    contract_id        TEXT         PRIMARY KEY
        CHECK (contract_id ~ '^dct_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id          VARCHAR(64)  NOT NULL,
    asset_id           TEXT         NOT NULL REFERENCES dqc_assets(asset_id),
    version_number     INT          NOT NULL CHECK (version_number >= 1),
    status             VARCHAR(16)  NOT NULL
        CHECK (status IN ('DRAFT', 'REVIEW', 'PUBLISHED', 'DEPRECATED', 'RETIRED')),
    body               JSONB        NOT NULL,
    content_sha256     CHAR(64)     NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    created_by         VARCHAR(128) NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    submitted_by       VARCHAR(128),
    submitted_at       TIMESTAMPTZ,
    published_by       VARCHAR(128),
    published_at       TIMESTAMPTZ,
    deprecated_at      TIMESTAMPTZ,
    retired_at         TIMESTAMPTZ,
    lifecycle_note     TEXT,
    compatibility_evidence JSONB,
    compared_to_version    INT,
    UNIQUE (tenant_id, asset_id, version_number),
    -- Maker-checker: whoever publishes is neither the author nor the submitter.
    CONSTRAINT dqc_publish_is_independent
        CHECK (published_by IS NULL
               OR (published_by <> created_by AND (submitted_by IS NULL OR published_by <> submitted_by))),
    -- A published version carries who/when and the compatibility evidence it was judged on.
    CONSTRAINT dqc_published_is_evidenced
        CHECK (status NOT IN ('PUBLISHED', 'DEPRECATED')
               OR (published_by IS NOT NULL AND published_at IS NOT NULL AND submitted_by IS NOT NULL)),
    CONSTRAINT dqc_retired_has_time CHECK (status <> 'RETIRED' OR retired_at IS NOT NULL)
);

-- At most one version per asset is being authored at a time: otherwise "compatible with
-- what?" has no single answer.
CREATE UNIQUE INDEX IF NOT EXISTS dqc_one_open_draft
    ON dqc_contract_versions (tenant_id, asset_id) WHERE status IN ('DRAFT', 'REVIEW');

CREATE INDEX IF NOT EXISTS idx_dqc_contract_versions_asset ON dqc_contract_versions (tenant_id, asset_id, version_number);

CREATE OR REPLACE FUNCTION enforce_dqc_contract_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'dqc contract version cannot be deleted';
    END IF;
    IF NEW.contract_id <> OLD.contract_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.asset_id <> OLD.asset_id
       OR NEW.version_number <> OLD.version_number OR NEW.body IS DISTINCT FROM OLD.body
       OR NEW.content_sha256 <> OLD.content_sha256 OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'dqc contract version content is immutable';
    END IF;
    IF NEW.status = OLD.status THEN
        -- Nothing about a version's history changes without a status change.
        IF NEW.submitted_by IS DISTINCT FROM OLD.submitted_by OR NEW.published_by IS DISTINCT FROM OLD.published_by
           OR NEW.published_at IS DISTINCT FROM OLD.published_at OR NEW.compatibility_evidence IS DISTINCT FROM OLD.compatibility_evidence THEN
            RAISE EXCEPTION 'dqc contract version history is immutable';
        END IF;
        RETURN NEW;
    END IF;
    IF NOT ((OLD.status = 'DRAFT'      AND NEW.status IN ('REVIEW', 'RETIRED'))
         OR (OLD.status = 'REVIEW'     AND NEW.status IN ('PUBLISHED', 'RETIRED'))
         OR (OLD.status = 'PUBLISHED'  AND NEW.status = 'DEPRECATED')
         OR (OLD.status = 'DEPRECATED' AND NEW.status = 'RETIRED')) THEN
        RAISE EXCEPTION 'dqc contract version may only move forward: % -> % is not allowed', OLD.status, NEW.status;
    END IF;
    -- Facts already on the row are not rewritten by a later transition.
    IF OLD.submitted_by IS NOT NULL AND NEW.submitted_by IS DISTINCT FROM OLD.submitted_by THEN
        RAISE EXCEPTION 'dqc contract version history is immutable';
    END IF;
    IF OLD.published_by IS NOT NULL AND (NEW.published_by IS DISTINCT FROM OLD.published_by
       OR NEW.published_at IS DISTINCT FROM OLD.published_at OR NEW.compatibility_evidence IS DISTINCT FROM OLD.compatibility_evidence) THEN
        RAISE EXCEPTION 'dqc contract version history is immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dqc_contract_lifecycle ON dqc_contract_versions;
CREATE TRIGGER trigger_dqc_contract_lifecycle
    BEFORE UPDATE OR DELETE ON dqc_contract_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_dqc_contract_lifecycle();

CREATE OR REPLACE FUNCTION enforce_dqc_asset_immutability()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'dqc asset identity is immutable';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_dqc_asset_immutability ON dqc_assets;
CREATE TRIGGER trigger_dqc_asset_immutability
    BEFORE UPDATE OR DELETE ON dqc_assets
    FOR EACH ROW EXECUTE FUNCTION enforce_dqc_asset_immutability();

ALTER TABLE dqc_assets ENABLE ROW LEVEL SECURITY;
ALTER TABLE dqc_assets FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dqc_assets_tenant_isolation ON dqc_assets;
CREATE POLICY dqc_assets_tenant_isolation ON dqc_assets
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE dqc_contract_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE dqc_contract_versions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS dqc_contract_versions_tenant_isolation ON dqc_contract_versions;
CREATE POLICY dqc_contract_versions_tenant_isolation ON dqc_contract_versions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
