-- 000006_scope_audit_and_activation_integrity.up.sql
--
-- Closes the PRV-01 audit gaps that need schema/RLS support:
--   * §8.2 gates 1/3/4/6 need fields the model did not have
--     (privacy_owner, necessity_rationale, special_condition_refs, processor_refs)
--   * §20.3 valid-time needs a closing bound (effective_to)
--   * lifecycle actor evidence must not depend on a droppable event
--     (activity_lifecycle_audit, append-only)
--   * platform (tenant_id IS NULL) records writable ONLY from an explicit
--     platform context (app.platform_scope), never by omitting the tenant
--   * registry history is retained: DELETE is rejected, not merely unpoliced
--   * outbox rows are no longer readable/writable from a tenant-less session

-- ── 1. Columns ──────────────────────────────────────────────────────────────
ALTER TABLE purpose_versions
    ADD COLUMN IF NOT EXISTS necessity_rationale    TEXT,
    ADD COLUMN IF NOT EXISTS special_condition_refs JSONB NOT NULL DEFAULT '[]';

ALTER TABLE processing_activity_versions
    ADD COLUMN IF NOT EXISTS privacy_owner  TEXT,
    ADD COLUMN IF NOT EXISTS processor_refs JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN IF NOT EXISTS effective_to   TIMESTAMPTZ;

-- Legacy RETIRED rows were closed with no end bound. Where a later version of
-- the same activity exists, that version's effective_from is the instant the
-- retired one stopped governing; where none exists the bound is unknowable and
-- is left NULL rather than invented.
UPDATE processing_activity_versions v
SET effective_to = s.next_from
FROM (
    SELECT v2.activity_version_id AS id,
           (SELECT MIN(n.effective_from)
              FROM processing_activity_versions n
             WHERE n.activity_id = v2.activity_id
               AND n.effective_from > v2.effective_from
               AND n.version_status IN ('ACTIVE', 'SUSPENDED', 'RETIRED')) AS next_from
      FROM processing_activity_versions v2
     WHERE v2.version_status = 'RETIRED'
       AND v2.effective_to IS NULL
       AND v2.effective_from IS NOT NULL
) s
WHERE v.activity_version_id = s.id AND s.next_from IS NOT NULL;

-- ── 2. Content-immutability trigger now covers the new content fields ───────
CREATE OR REPLACE FUNCTION reject_activity_content_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.version_status <> 'DRAFT' THEN
        IF NEW.activity_id IS DISTINCT FROM OLD.activity_id
           OR NEW.privacy_role IS DISTINCT FROM OLD.privacy_role
           OR NEW.owner IS DISTINCT FROM OLD.owner
           OR NEW.privacy_owner IS DISTINCT FROM OLD.privacy_owner
           OR NEW.purpose_ids IS DISTINCT FROM OLD.purpose_ids
           OR NEW.subject_classes IS DISTINCT FROM OLD.subject_classes
           OR NEW.data_categories IS DISTINCT FROM OLD.data_categories
           OR NEW.sources IS DISTINCT FROM OLD.sources
           OR NEW.recipients IS DISTINCT FROM OLD.recipients
           OR NEW.jurisdictions IS DISTINCT FROM OLD.jurisdictions
           OR NEW.retention_rule_refs IS DISTINCT FROM OLD.retention_rule_refs
           OR NEW.transfer_refs IS DISTINCT FROM OLD.transfer_refs
           OR NEW.processor_refs IS DISTINCT FROM OLD.processor_refs
           OR NEW.notice_consent_dependency IS DISTINCT FROM OLD.notice_consent_dependency
           OR NEW.dpia_tia_status IS DISTINCT FROM OLD.dpia_tia_status
           OR NEW.supersedes_version_id IS DISTINCT FROM OLD.supersedes_version_id
           OR NEW.created_at IS DISTINCT FROM OLD.created_at
           OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        THEN
            RAISE EXCEPTION 'processing_activity_versions content is immutable once it leaves DRAFT: only version_status/validation_findings/rejection_reason/effective_from/effective_to and lifecycle evidence fields may change (row %)', OLD.activity_version_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── 3. History is retained: DELETE is rejected on every registry table ──────
CREATE OR REPLACE FUNCTION reject_registry_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is retained governance history: DELETE is never permitted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER purposes_no_delete
    BEFORE DELETE ON purposes
    FOR EACH ROW EXECUTE FUNCTION reject_registry_delete();
CREATE TRIGGER purpose_versions_no_delete
    BEFORE DELETE ON purpose_versions
    FOR EACH ROW EXECUTE FUNCTION reject_registry_delete();
CREATE TRIGGER processing_activities_no_delete
    BEFORE DELETE ON processing_activities
    FOR EACH ROW EXECUTE FUNCTION reject_registry_delete();
CREATE TRIGGER processing_activity_versions_no_delete
    BEFORE DELETE ON processing_activity_versions
    FOR EACH ROW EXECUTE FUNCTION reject_registry_delete();

-- ── 4. Lifecycle audit evidence (append-only) ───────────────────────────────
CREATE TABLE IF NOT EXISTS activity_lifecycle_audit (
    audit_id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID,
    activity_id              UUID        NOT NULL REFERENCES processing_activities(activity_id),
    activity_version_id      UUID        NOT NULL REFERENCES processing_activity_versions(activity_version_id),
    action                   VARCHAR(32) NOT NULL,
    from_status              VARCHAR(32),
    to_status                VARCHAR(32) NOT NULL,
    actor_principal_id       TEXT        NOT NULL CHECK (actor_principal_id <> ''),
    correlation_id           TEXT        NOT NULL DEFAULT '',
    reason                   TEXT,
    superseded_by_version_id UUID,
    effective_from           TIMESTAMPTZ,
    effective_to             TIMESTAMPTZ,
    recorded_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_activity_lifecycle_audit_activity
    ON activity_lifecycle_audit (activity_id, recorded_at);

CREATE OR REPLACE FUNCTION reject_audit_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only evidence: % is never permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER activity_lifecycle_audit_append_only
    BEFORE UPDATE OR DELETE ON activity_lifecycle_audit
    FOR EACH ROW EXECUTE FUNCTION reject_audit_mutation();

-- ── 5. RLS: writes need a matching tenant OR the explicit platform capability
--
-- app.platform_scope = 'true' is set by the store ONLY when the request
-- carried the platform-scope sentinel in X-Tenant-Id. A session with no
-- tenant and no capability can read platform rows (SELECT is unchanged) but
-- can write nothing. A platform session can never write a tenant row.
--
-- Write predicate for a row owned by tenant T (T may be NULL = platform):
--   (T IS NOT NULL AND platform_scope <> 'true' AND T = app.tenant_id)
--   OR (T IS NULL AND platform_scope = 'true')

-- purposes ------------------------------------------------------------------
DROP POLICY IF EXISTS tenant_insert_policy ON purposes;
DROP POLICY IF EXISTS tenant_update_policy ON purposes;
DROP POLICY IF EXISTS tenant_delete_policy ON purposes;
CREATE POLICY tenant_insert_policy ON purposes
    FOR INSERT
    WITH CHECK (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    );
CREATE POLICY tenant_update_policy ON purposes
    FOR UPDATE
    USING (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    )
    WITH CHECK (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    );

-- processing_activities -----------------------------------------------------
DROP POLICY IF EXISTS tenant_insert_policy ON processing_activities;
DROP POLICY IF EXISTS tenant_update_policy ON processing_activities;
DROP POLICY IF EXISTS tenant_delete_policy ON processing_activities;
CREATE POLICY tenant_insert_policy ON processing_activities
    FOR INSERT
    WITH CHECK (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    );
CREATE POLICY tenant_update_policy ON processing_activities
    FOR UPDATE
    USING (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    )
    WITH CHECK (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    );

-- purpose_versions (ownership via the parent purpose) -----------------------
DROP POLICY IF EXISTS tenant_insert_policy ON purpose_versions;
DROP POLICY IF EXISTS tenant_update_policy ON purpose_versions;
DROP POLICY IF EXISTS tenant_delete_policy ON purpose_versions;
CREATE POLICY tenant_insert_policy ON purpose_versions
    FOR INSERT
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (
                  (p.tenant_id IS NOT NULL
                   AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
                   AND p.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                  OR (p.tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
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
                  (p.tenant_id IS NOT NULL
                   AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
                   AND p.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                  OR (p.tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM purposes p
            WHERE p.purpose_id = purpose_versions.purpose_id
              AND (
                  (p.tenant_id IS NOT NULL
                   AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
                   AND p.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                  OR (p.tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
              )
        )
    );

-- processing_activity_versions (ownership via the parent activity) ----------
DROP POLICY IF EXISTS tenant_insert_policy ON processing_activity_versions;
DROP POLICY IF EXISTS tenant_update_policy ON processing_activity_versions;
DROP POLICY IF EXISTS tenant_delete_policy ON processing_activity_versions;
CREATE POLICY tenant_insert_policy ON processing_activity_versions
    FOR INSERT
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (
                  (a.tenant_id IS NOT NULL
                   AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
                   AND a.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                  OR (a.tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
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
                  (a.tenant_id IS NOT NULL
                   AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
                   AND a.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                  OR (a.tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
              )
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM processing_activities a
            WHERE a.activity_id = processing_activity_versions.activity_id
              AND (
                  (a.tenant_id IS NOT NULL
                   AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
                   AND a.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                  OR (a.tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
              )
        )
    );

-- activity_lifecycle_audit: own tenant (or platform capability) only ---------
ALTER TABLE activity_lifecycle_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE activity_lifecycle_audit FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON activity_lifecycle_audit
    FOR ALL
    USING (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    )
    WITH CHECK (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    );

-- idempotency keys: cached response bodies must not cross scopes ------------
DROP POLICY IF EXISTS tenant_isolation_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_select_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_insert_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_update_policy ON purpose_registry_idempotency_keys;
DROP POLICY IF EXISTS tenant_delete_policy ON purpose_registry_idempotency_keys;
CREATE POLICY tenant_isolation_policy ON purpose_registry_idempotency_keys
    FOR ALL
    USING (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    )
    WITH CHECK (
        (tenant_id IS NOT NULL
         AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
         AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        OR (tenant_id IS NULL AND current_setting('app.platform_scope', true) = 'true')
    );

-- event_outbox: the old policy let ANY session with an empty tenant read and
-- write every tenant's pending events. Now: the relay capability, the owning
-- tenant, or (for platform events, tenant_id = '') the platform capability.
DROP POLICY IF EXISTS tenant_isolation_policy ON event_outbox;
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        current_setting('app.outbox_relay', true) = 'true'
        OR (tenant_id <> ''
            AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
            AND tenant_id = COALESCE(current_setting('app.tenant_id', true), ''))
        OR (tenant_id = '' AND current_setting('app.platform_scope', true) = 'true')
    )
    WITH CHECK (
        current_setting('app.outbox_relay', true) = 'true'
        OR (tenant_id <> ''
            AND COALESCE(current_setting('app.platform_scope', true), '') <> 'true'
            AND tenant_id = COALESCE(current_setting('app.tenant_id', true), ''))
        OR (tenant_id = '' AND current_setting('app.platform_scope', true) = 'true')
    );
