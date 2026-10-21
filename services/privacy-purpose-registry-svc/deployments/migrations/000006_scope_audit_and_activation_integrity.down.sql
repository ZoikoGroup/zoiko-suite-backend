-- 000006_scope_audit_and_activation_integrity.down.sql
-- Restores the 000005 policy set and removes everything 000006 added.
-- Data in the dropped columns/tables is lost by design of a down migration.

DROP TRIGGER IF EXISTS processing_activity_versions_no_delete ON processing_activity_versions;
DROP TRIGGER IF EXISTS processing_activities_no_delete ON processing_activities;
DROP TRIGGER IF EXISTS purpose_versions_no_delete ON purpose_versions;
DROP TRIGGER IF EXISTS purposes_no_delete ON purposes;
DROP FUNCTION IF EXISTS reject_registry_delete();

DROP TABLE IF EXISTS activity_lifecycle_audit;
DROP FUNCTION IF EXISTS reject_audit_mutation();

-- 000005 trigger function (without the 000006 content fields)
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
            RAISE EXCEPTION 'processing_activity_versions content is immutable once it leaves DRAFT (row %)', OLD.activity_version_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE processing_activity_versions
    DROP COLUMN IF EXISTS effective_to,
    DROP COLUMN IF EXISTS processor_refs,
    DROP COLUMN IF EXISTS privacy_owner;

ALTER TABLE purpose_versions
    DROP COLUMN IF EXISTS special_condition_refs,
    DROP COLUMN IF EXISTS necessity_rationale;

-- 000005 write policies on the parent tables
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['purposes', 'processing_activities'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_insert_policy ON %I', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_update_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_insert_policy ON %I FOR INSERT WITH CHECK (
            (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
            OR (tenant_id::text = current_setting('app.tenant_id', true)))$p$, t);
        EXECUTE format($p$CREATE POLICY tenant_update_policy ON %I FOR UPDATE USING (
            (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
            OR (tenant_id::text = current_setting('app.tenant_id', true)))
            WITH CHECK (
            (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
            OR (tenant_id::text = current_setting('app.tenant_id', true)))$p$, t);
        EXECUTE format($p$CREATE POLICY tenant_delete_policy ON %I FOR DELETE USING (
            (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND tenant_id IS NULL)
            OR (tenant_id::text = current_setting('app.tenant_id', true)))$p$, t);
    END LOOP;
END $$;

-- 000005 write policies on the version tables (ownership via parent)
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT * FROM (VALUES
        ('purpose_versions', 'purposes', 'purpose_id'),
        ('processing_activity_versions', 'processing_activities', 'activity_id')
    ) AS v(child, parent, fk) LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_insert_policy ON %I', r.child);
        EXECUTE format('DROP POLICY IF EXISTS tenant_update_policy ON %I', r.child);
        EXECUTE format($p$CREATE POLICY tenant_insert_policy ON %1$I FOR INSERT WITH CHECK (EXISTS (
            SELECT 1 FROM %2$I p WHERE p.%3$I = %1$I.%3$I AND (
              (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
              OR p.tenant_id::text = current_setting('app.tenant_id', true))))$p$, r.child, r.parent, r.fk);
        EXECUTE format($p$CREATE POLICY tenant_update_policy ON %1$I FOR UPDATE USING (EXISTS (
            SELECT 1 FROM %2$I p WHERE p.%3$I = %1$I.%3$I AND (
              (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
              OR p.tenant_id::text = current_setting('app.tenant_id', true))))
            WITH CHECK (EXISTS (
            SELECT 1 FROM %2$I p WHERE p.%3$I = %1$I.%3$I AND (
              (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
              OR p.tenant_id::text = current_setting('app.tenant_id', true))))$p$, r.child, r.parent, r.fk);
        EXECUTE format($p$CREATE POLICY tenant_delete_policy ON %1$I FOR DELETE USING (EXISTS (
            SELECT 1 FROM %2$I p WHERE p.%3$I = %1$I.%3$I AND (
              (NULLIF(current_setting('app.tenant_id', true), '') IS NULL AND p.tenant_id IS NULL)
              OR p.tenant_id::text = current_setting('app.tenant_id', true))))$p$, r.child, r.parent, r.fk);
    END LOOP;
END $$;

-- 000004/000005 idempotency policy
DROP POLICY IF EXISTS tenant_isolation_policy ON purpose_registry_idempotency_keys;
CREATE POLICY tenant_isolation_policy ON purpose_registry_idempotency_keys
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- 000005 outbox policy
DROP POLICY IF EXISTS tenant_isolation_policy ON event_outbox;
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
