-- 000005_fix_gaps.up.sql
-- Resolves the 6 confirmed gaps for privacy-purpose-registry-svc (PRV-01):
-- 1. One ACTIVE version per activity (§9.2): partial unique index.
-- 2. Platform tenant isolation (§23.1): remove unsafe 'tenant_id IS NULL OR ...' on write.
-- 3. Transactional Event Outbox (§19.1): durable outbox table with RLS and retry support.
-- 4. Lifecycle evidence (§8.1): approver, rejecter, activator, and publisher audit columns.

-- ── 1. One ACTIVE version per activity unique constraint (§9.2) ─────────────
CREATE UNIQUE INDEX IF NOT EXISTS uq_activity_versions_one_active
    ON processing_activity_versions (activity_id)
    WHERE version_status = 'ACTIVE';

-- ── 2. Platform tenant isolation (§23.1) ────────────────────────────────────
-- Separate SELECT from INSERT/UPDATE/DELETE so normal tenants can READ platform
-- records (tenant_id IS NULL), but CANNOT write, update, or delete them.

-- Table: purposes
DROP POLICY IF EXISTS tenant_isolation_policy ON purposes;
DROP POLICY IF EXISTS tenant_select_policy ON purposes;
DROP POLICY IF EXISTS tenant_insert_policy ON purposes;
DROP POLICY IF EXISTS tenant_update_policy ON purposes;
DROP POLICY IF EXISTS tenant_delete_policy ON purposes;

CREATE POLICY tenant_select_policy ON purposes
    FOR SELECT
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );

CREATE POLICY tenant_insert_policy ON purposes
    FOR INSERT
    WITH CHECK (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_update_policy ON purposes
    FOR UPDATE
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    )
    WITH CHECK (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_delete_policy ON purposes
    FOR DELETE
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

-- Table: purpose_versions
DROP POLICY IF EXISTS tenant_isolation_policy ON purpose_versions;
DROP POLICY IF EXISTS tenant_select_policy ON purpose_versions;
DROP POLICY IF EXISTS tenant_insert_policy ON purpose_versions;
DROP POLICY IF EXISTS tenant_update_policy ON purpose_versions;
DROP POLICY IF EXISTS tenant_delete_policy ON purpose_versions;

CREATE POLICY tenant_select_policy ON purpose_versions
    FOR SELECT
    USING (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (p.tenant_id IS NULL OR p.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    );

CREATE POLICY tenant_insert_policy ON purpose_versions
    FOR INSERT
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
                  OR (p.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    );

CREATE POLICY tenant_update_policy ON purpose_versions
    FOR UPDATE
    USING (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
                  OR (p.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
                  OR (p.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    );

CREATE POLICY tenant_delete_policy ON purpose_versions
    FOR DELETE
    USING (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
                  OR (p.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    );

-- Table: processing_activities
DROP POLICY IF EXISTS tenant_isolation_policy ON processing_activities;
DROP POLICY IF EXISTS tenant_select_policy ON processing_activities;
DROP POLICY IF EXISTS tenant_insert_policy ON processing_activities;
DROP POLICY IF EXISTS tenant_update_policy ON processing_activities;
DROP POLICY IF EXISTS tenant_delete_policy ON processing_activities;

CREATE POLICY tenant_select_policy ON processing_activities
    FOR SELECT
    USING (
        tenant_id IS NULL
        OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
    );

CREATE POLICY tenant_insert_policy ON processing_activities
    FOR INSERT
    WITH CHECK (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_update_policy ON processing_activities
    FOR UPDATE
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    )
    WITH CHECK (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_delete_policy ON processing_activities
    FOR DELETE
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

-- Table: processing_activity_versions
DROP POLICY IF EXISTS tenant_isolation_policy ON processing_activity_versions;
DROP POLICY IF EXISTS tenant_select_policy ON processing_activity_versions;
DROP POLICY IF EXISTS tenant_insert_policy ON processing_activity_versions;
DROP POLICY IF EXISTS tenant_update_policy ON processing_activity_versions;
DROP POLICY IF EXISTS tenant_delete_policy ON processing_activity_versions;

CREATE POLICY tenant_select_policy ON processing_activity_versions
    FOR SELECT
    USING (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (a.tenant_id IS NULL OR a.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        )
    );

CREATE POLICY tenant_insert_policy ON processing_activity_versions
    FOR INSERT
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND a.tenant_id IS NULL)
                  OR (a.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    );

CREATE POLICY tenant_update_policy ON processing_activity_versions
    FOR UPDATE
    USING (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND a.tenant_id IS NULL)
                  OR (a.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND a.tenant_id IS NULL)
                  OR (a.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    );

CREATE POLICY tenant_delete_policy ON processing_activity_versions
    FOR DELETE
    USING (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (
                  (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND a.tenant_id IS NULL)
                  OR (a.tenant_id::text = current_setting('app.tenant_id', true))
              )
        )
    );

-- Table: purpose_registry_idempotency_keys
DROP POLICY IF EXISTS tenant_isolation_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_select_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_insert_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_update_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_delete_policy ON purpose_registry_idempotency_keys;

CREATE POLICY tenant_select_policy ON purpose_registry_idempotency_keys
    FOR SELECT
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_insert_policy ON purpose_registry_idempotency_keys
    FOR INSERT
    WITH CHECK (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_update_policy ON purpose_registry_idempotency_keys
    FOR UPDATE
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    )
    WITH CHECK (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

CREATE POLICY tenant_delete_policy ON purpose_registry_idempotency_keys
    FOR DELETE
    USING (
        (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
        OR (tenant_id::text = current_setting('app.tenant_id', true))
    );

-- ── 3. Lifecycle Evidence Audit Columns (§8.1) ──────────────────────────────
ALTER TABLE processing_activity_versions
    ADD COLUMN IF NOT EXISTS approved_by_principal_id TEXT,
    ADD COLUMN IF NOT EXISTS approved_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rejected_by_principal_id TEXT,
    ADD COLUMN IF NOT EXISTS rejected_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS activated_by_principal_id TEXT,
    ADD COLUMN IF NOT EXISTS activated_at              TIMESTAMPTZ;

ALTER TABLE purpose_versions
    ADD COLUMN IF NOT EXISTS published_by_principal_id TEXT,
    ADD COLUMN IF NOT EXISTS published_at               TIMESTAMPTZ;

-- Update trigger function to allow lifecycle audit timestamps/principals to update post-DRAFT
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
            RAISE EXCEPTION 'processing_activity_versions content is immutable once it leaves DRAFT: only version_status/validation_findings/rejection_reason/effective_from and lifecycle evidence fields may change (row %)', OLD.activity_version_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── 4. Transactional Event Outbox (§19.1) ───────────────────────────────────
CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_event_id VARCHAR(255) PRIMARY KEY,
    event_type      VARCHAR(200) NOT NULL,
    tenant_id       VARCHAR(255) NOT NULL DEFAULT '',
    entity_id       VARCHAR(255) NOT NULL DEFAULT '',
    actor_id        VARCHAR(255) NOT NULL DEFAULT '',
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    payload         JSONB        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    attempts        INT          NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_error      TEXT,

    CONSTRAINT event_outbox_attempts_nonneg CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        tenant_id = COALESCE(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
        OR current_setting('app.tenant_id', true) = ''
    )
    WITH CHECK (
        tenant_id = COALESCE(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
        OR current_setting('app.tenant_id', true) = ''
    );
