-- 000036: separation-of-duties and ABAC rules are published under
-- maker-checker (AU-17; ZS-IAM-001 §8 PAP "no direct runtime mutation without
-- controlled publication"; Governance Control Plane §1 "independently
-- approved"). Decision 22 Oct 2026: SoD + ABAC rules; roles and permission
-- bundles keep coming through access-control-svc's own governed flow.
--
-- A rule took effect the moment it was created, and retire / reactivate the
-- moment they were called: versioned and evented, but never independently
-- approved. Now every change to what a rule enforces is a REQUEST:
--
--   pending_change  PUBLISH     a new rule; inactive until approved.
--                   REACTIVATE  restore a retired rule; still retired until approved.
--                   RETIRE      switch a rule off; it keeps denying until approved.
--   approve         applies the change (active_flag), publication_status
--                   PUBLISHED, published_by / published_at — never the
--                   requester (trigger below).
--   reject          clears the request; a rejected PUBLISH leaves the rule
--                   REJECTED, which can never be activated.
--
-- Rules that exist before this migration (the 000019 baseline and anything
-- authored since) were in force already and stay PUBLISHED.
--
-- Publication events change with it: iam.sod_policy.published and
-- iam.policy_set.published are emitted when a change is APPROVED, not when it
-- is requested — a pending rule has not been published.

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['sod_rules', 'abac_rules'] LOOP
        EXECUTE format($f$
            ALTER TABLE %1$I
                ADD COLUMN IF NOT EXISTS publication_status  VARCHAR(20) NOT NULL DEFAULT 'PUBLISHED',
                ADD COLUMN IF NOT EXISTS pending_change      VARCHAR(16),
                ADD COLUMN IF NOT EXISTS change_requested_by TEXT,
                ADD COLUMN IF NOT EXISTS change_requested_at TIMESTAMPTZ,
                ADD COLUMN IF NOT EXISTS change_reason       TEXT,
                ADD COLUMN IF NOT EXISTS published_by        TEXT,
                ADD COLUMN IF NOT EXISTS published_at        TIMESTAMPTZ,
                ADD COLUMN IF NOT EXISTS rejected_by         TEXT,
                ADD COLUMN IF NOT EXISTS rejected_at         TIMESTAMPTZ
        $f$, t);
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_publication_status_check');
        EXECUTE format($f$ALTER TABLE %I ADD CONSTRAINT %I
            CHECK (publication_status IN ('PENDING_APPROVAL', 'PUBLISHED', 'REJECTED'))$f$, t, t || '_publication_status_check');
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_pending_change_check');
        -- A pending change and the state it applies to belong together; a
        -- REJECTED rule is never active.
        EXECUTE format($f$ALTER TABLE %I ADD CONSTRAINT %I CHECK (
            (pending_change IS NULL AND publication_status <> 'PENDING_APPROVAL')
            OR (pending_change = 'PUBLISH' AND publication_status = 'PENDING_APPROVAL' AND NOT active_flag)
            OR (pending_change = 'REACTIVATE' AND publication_status = 'PUBLISHED' AND NOT active_flag)
            OR (pending_change = 'RETIRE' AND publication_status = 'PUBLISHED' AND active_flag))
            $f$, t, t || '_pending_change_check');
        EXECUTE format('ALTER TABLE %I DROP CONSTRAINT IF EXISTS %I', t, t || '_rejected_inactive_check');
        EXECUTE format($f$ALTER TABLE %I ADD CONSTRAINT %I
            CHECK (publication_status <> 'REJECTED' OR NOT active_flag)$f$, t, t || '_rejected_inactive_check');
    END LOOP;
END
$$;

-- The checker is never the maker: checked when a decision is recorded.
CREATE OR REPLACE FUNCTION authz_rule_publication_independent() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.published_at IS DISTINCT FROM OLD.published_at
       AND NEW.published_by IS NOT DISTINCT FROM OLD.change_requested_by THEN
        RAISE EXCEPTION 'a rule change is approved by someone other than its requester'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.rejected_at IS DISTINCT FROM OLD.rejected_at
       AND NEW.rejected_by IS NOT DISTINCT FROM OLD.change_requested_by THEN
        RAISE EXCEPTION 'a rule change is rejected by someone other than its requester'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS sod_rules_publication_independent ON sod_rules;
CREATE TRIGGER sod_rules_publication_independent
    BEFORE UPDATE ON sod_rules
    FOR EACH ROW EXECUTE FUNCTION authz_rule_publication_independent();
DROP TRIGGER IF EXISTS abac_rules_publication_independent ON abac_rules;
CREATE TRIGGER abac_rules_publication_independent
    BEFORE UPDATE ON abac_rules
    FOR EACH ROW EXECUTE FUNCTION authz_rule_publication_independent();

-- Publication events: emitted when a configuration change takes effect.
-- A snapshot still awaiting approval (or rejected) has published nothing.
CREATE OR REPLACE FUNCTION authz_config_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_version TEXT := 'cfg.' || NEW.history_id;
    v_state   TEXT := COALESCE(NEW.snapshot->>'publication_status', NEW.snapshot->>'status', '');
    v_payload JSONB := jsonb_build_object(
        'object_type', NEW.object_type, 'object_id', NEW.object_id, 'version', NEW.version,
        'snapshot', NEW.snapshot, 'changed_by', NEW.changed_by, 'reason', NEW.reason,
        'policy_set_version', v_version);
BEGIN
    IF v_state IN ('PENDING_APPROVAL', 'REJECTED') THEN
        RETURN NULL;
    END IF;
    -- Neither does a new request against something in force, nor the
    -- rejection of one: what is enforced did not change.
    IF NEW.object_type IN ('sod_rules', 'abac_rules') AND (
           NEW.snapshot->>'pending_change' IS NOT NULL
        OR (NEW.snapshot->>'rejected_at' IS NOT NULL
            AND (NEW.snapshot->>'published_at' IS NULL
                 OR (NEW.snapshot->>'rejected_at')::timestamptz > (NEW.snapshot->>'published_at')::timestamptz))) THEN
        RETURN NULL;
    END IF;
    IF NEW.object_type = 'authority_limits' AND (
           NEW.snapshot->>'pending_action' IS NOT NULL OR NEW.snapshot->>'last_decision' = 'REJECTED') THEN
        RETURN NULL;
    END IF;
    IF NEW.object_type IN ('roles', 'permission_bundles') THEN
        PERFORM authz_emit_event('iam.role.published', NEW.object_id::text, NEW.tenant_id, NULL, v_payload);
    ELSIF NEW.object_type = 'sod_rules' THEN
        PERFORM authz_emit_event('iam.sod_policy.published', NEW.object_id::text, NEW.tenant_id, NULL, v_payload);
    END IF;
    PERFORM authz_emit_event('iam.policy_set.published', v_version, NEW.tenant_id, NULL,
        jsonb_build_object('policy_set_version', v_version, 'object_type', NEW.object_type,
                           'object_id', NEW.object_id, 'published_by', NEW.changed_by));
    RETURN NULL;
END;
$$;

-- A rule change request is evidence too: requested / rejected events, so the
-- governance log sees the request and its outcome, not only publications.
CREATE OR REPLACE FUNCTION authz_rule_change_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_id     TEXT := CASE WHEN TG_TABLE_NAME = 'sod_rules' THEN to_jsonb(NEW)->>'sod_rule_id' ELSE to_jsonb(NEW)->>'abac_rule_id' END;
    v_prefix TEXT := CASE WHEN TG_TABLE_NAME = 'sod_rules' THEN 'iam.sod_rule' ELSE 'iam.abac_rule' END;
BEGIN
    IF NEW.pending_change IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.pending_change IS NULL) THEN
        PERFORM authz_emit_event(v_prefix || '.change_requested', v_id, NEW.tenant_id, NULL,
            to_jsonb(NEW) || jsonb_build_object('change', NEW.pending_change));
    ELSIF TG_OP = 'UPDATE' AND NEW.rejected_at IS DISTINCT FROM OLD.rejected_at THEN
        PERFORM authz_emit_event(v_prefix || '.change_rejected', v_id, NEW.tenant_id, NULL,
            to_jsonb(NEW) || jsonb_build_object('change', OLD.pending_change));
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS sod_rules_change_events ON sod_rules;
CREATE TRIGGER sod_rules_change_events
    AFTER INSERT OR UPDATE ON sod_rules
    FOR EACH ROW EXECUTE FUNCTION authz_rule_change_events();
DROP TRIGGER IF EXISTS abac_rules_change_events ON abac_rules;
CREATE TRIGGER abac_rules_change_events
    AFTER INSERT OR UPDATE ON abac_rules
    FOR EACH ROW EXECUTE FUNCTION authz_rule_change_events();
