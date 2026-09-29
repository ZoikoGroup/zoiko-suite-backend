-- 000004_activation_gates_and_idempotency.up.sql
-- Enforces PRV-01 Activation Gates 7 & 8 (§8.2) and Idempotency key tracking (§18.1).

ALTER TABLE processing_activity_versions
    ADD COLUMN IF NOT EXISTS notice_consent_dependency VARCHAR(32),
    ADD COLUMN IF NOT EXISTS dpia_tia_status VARCHAR(32);

-- Update immutability trigger to protect the new content columns once a version leaves DRAFT
CREATE OR REPLACE FUNCTION reject_activity_content_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.version_status <> 'DRAFT' THEN
        IF NEW.activity_id IS DISTINCT FROM OLD.activity_id
           OR NEW.privacy_role IS DISTINCT FROM OLD.privacy_role
           OR NEW.owner IS DISTINCT FROM OLD.owner
           OR NEW.purpose_ids IS DISTINCT FROM OLD.purpose_ids
           OR NEW.subject_classes IS DISTINCT FROM OLD.subject_classes
           OR NEW.data_categories IS DISTINCT FROM OLD.data_categories
           OR NEW.sources IS DISTINCT FROM OLD.sources
           OR NEW.recipients IS DISTINCT FROM OLD.recipients
           OR NEW.jurisdictions IS DISTINCT FROM OLD.jurisdictions
           OR NEW.retention_rule_refs IS DISTINCT FROM OLD.retention_rule_refs
           OR NEW.transfer_refs IS DISTINCT FROM OLD.transfer_refs
           OR NEW.notice_consent_dependency IS DISTINCT FROM OLD.notice_consent_dependency
           OR NEW.dpia_tia_status IS DISTINCT FROM OLD.dpia_tia_status
           OR NEW.supersedes_version_id IS DISTINCT FROM OLD.supersedes_version_id
           OR NEW.created_at IS DISTINCT FROM OLD.created_at
           OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        THEN
            RAISE EXCEPTION 'processing_activity_versions content is immutable once it leaves DRAFT: only version_status/validation_findings/rejection_reason/effective_from may change (row %)', OLD.activity_version_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Idempotency storage (§18.1)
CREATE TABLE IF NOT EXISTS purpose_registry_idempotency_keys (
    idempotency_key          TEXT        NOT NULL,
    tenant_id                UUID,
    principal_id             TEXT        NOT NULL,
    operation                TEXT        NOT NULL,
    request_hash             TEXT        NOT NULL,
    response_status          INT         NOT NULL,
    response_body            JSONB       NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Unique index allowing NULL tenant_id (platform scope)
CREATE UNIQUE INDEX IF NOT EXISTS idx_purpose_registry_idempotency_unique
    ON purpose_registry_idempotency_keys (COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), principal_id, idempotency_key);

ALTER TABLE purpose_registry_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE purpose_registry_idempotency_keys FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON purpose_registry_idempotency_keys
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );
