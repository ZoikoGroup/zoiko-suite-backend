-- 000026: the IAM domain events (ZS-IAM-001 §23; GOV-03 Events), emitted from
-- the database into the outbox in the same transaction as the change.
--
-- No admin write published anything, so grants, revocations and rule changes
-- were invisible to audit and to the other replicas' caches. Triggers rather
-- than handler code for the reason the 000022 history is: every write path —
-- including one added later — emits, and the event commits with the change or
-- not at all (Governance Control Plane invariant #10). The relay (000020)
-- publishes them at least once.
--
--   principal_role_assignments  iam.assignment.granted  (insert APPROVED, or approved later)
--                               iam.assignment.revoked  (effective_to set or brought earlier)
--   delegated_authorities       iam.delegation.granted / iam.delegation.revoked
--                               (locally created only — a projection of
--                               delegated-authority-svc is that service's event)
--   roles, permission_bundles   iam.role.published      (role version activated)
--   sod_rules                   iam.sod_policy.published
--   every configuration change  iam.policy_set.published with the new
--                               policy_set_version (GOV-03 AuthorizationPolicyVersionChanged)
--
-- The envelope matches internal/events (Doc 03 §19), with tenant, actor and
-- correlation from the app.* settings the store installs on every transaction.

CREATE OR REPLACE FUNCTION authz_emit_event(p_type TEXT, p_key TEXT, p_tenant UUID, p_entity TEXT, p_payload JSONB)
RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO outbox_events (event_type, message_key, message_value, tenant_id)
    VALUES (p_type, p_key,
        jsonb_strip_nulls(jsonb_build_object(
            'event_id',       'evt-' || gen_random_uuid()::text,
            'event_type',     p_type,
            'event_version',  '1.0',
            'emitted_at',     to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
            'schema_version', '1.0',
            'source_service', 'authorization-svc',
            'tenant_id',      p_tenant,
            'legal_entity_id', NULLIF(p_entity, ''),
            'actor_id',       NULLIF(current_setting('app.actor_id', true), ''),
            'correlation_id', COALESCE(current_setting('app.correlation_id', true), ''),
            'payload',        p_payload)),
        p_tenant);
END;
$$;

CREATE OR REPLACE FUNCTION authz_assignment_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_tenant UUID;
    v_payload JSONB;
BEGIN
    SELECT r.tenant_id INTO v_tenant FROM roles r WHERE r.role_id = NEW.role_id;
    v_payload := jsonb_build_object(
        'principal_role_assignment_id', NEW.principal_role_assignment_id,
        'principal_id', NEW.principal_id, 'role_id', NEW.role_id,
        'legal_entity_id', NEW.legal_entity_id, 'book_id', NEW.book_id, 'org_unit_id', NEW.org_unit_id,
        'effective_from', NEW.effective_from, 'effective_to', NEW.effective_to,
        'assigned_by', NEW.assigned_by, 'approved_by', NEW.approved_by,
        'approval_reference', NEW.approval_reference, 'approval_status', NEW.approval_status);
    IF TG_OP = 'INSERT' THEN
        IF NEW.approval_status = 'APPROVED' THEN
            PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
        END IF;
        RETURN NULL;
    END IF;
    IF NEW.approval_status = 'APPROVED' AND OLD.approval_status IS DISTINCT FROM 'APPROVED' THEN
        PERFORM authz_emit_event('iam.assignment.granted', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    IF NEW.approval_status = 'APPROVED' AND NEW.effective_to IS DISTINCT FROM OLD.effective_to AND NEW.effective_to IS NOT NULL THEN
        PERFORM authz_emit_event('iam.assignment.revoked', NEW.principal_role_assignment_id::text, v_tenant, NEW.legal_entity_id::text, v_payload);
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS pra_iam_events ON principal_role_assignments;
CREATE TRIGGER pra_iam_events
    AFTER INSERT OR UPDATE ON principal_role_assignments
    FOR EACH ROW EXECUTE FUNCTION authz_assignment_events();

CREATE OR REPLACE FUNCTION authz_delegation_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_payload JSONB;
BEGIN
    IF NEW.source_service IS NOT NULL THEN
        RETURN NULL; -- a projection of delegated-authority-svc: its own event
    END IF;
    v_payload := jsonb_build_object(
        'delegated_authority_id', NEW.delegated_authority_id,
        'delegator_principal_id', NEW.delegator_principal_id, 'delegate_principal_id', NEW.delegate_principal_id,
        'scope_type', NEW.scope_type, 'legal_entity_id', NEW.legal_entity_id,
        'delegated_actions', NEW.delegated_actions,
        'effective_from', NEW.effective_from, 'effective_to', NEW.effective_to,
        'revocation_status', NEW.revocation_status, 'reason', NEW.reason, 'approval_reference', NEW.approval_reference);
    IF TG_OP = 'INSERT' THEN
        PERFORM authz_emit_event('iam.delegation.granted', NEW.delegated_authority_id::text, NEW.tenant_id, NEW.legal_entity_id::text, v_payload);
    ELSIF NEW.revocation_status IS DISTINCT FROM OLD.revocation_status AND NEW.revocation_status <> 'ACTIVE' THEN
        PERFORM authz_emit_event('iam.delegation.revoked', NEW.delegated_authority_id::text, NEW.tenant_id, NEW.legal_entity_id::text, v_payload);
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS da_iam_events ON delegated_authorities;
CREATE TRIGGER da_iam_events
    AFTER INSERT OR UPDATE ON delegated_authorities
    FOR EACH ROW EXECUTE FUNCTION authz_delegation_events();

-- Configuration events ride on the history row: one per real change, after
-- the version bump, carrying the snapshot and the new policy_set_version.
CREATE OR REPLACE FUNCTION authz_config_events() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_version TEXT := 'cfg.' || NEW.history_id;
    v_payload JSONB := jsonb_build_object(
        'object_type', NEW.object_type, 'object_id', NEW.object_id, 'version', NEW.version,
        'snapshot', NEW.snapshot, 'changed_by', NEW.changed_by, 'reason', NEW.reason,
        'policy_set_version', v_version);
BEGIN
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

DROP TRIGGER IF EXISTS authz_config_history_events ON authz_config_history;
CREATE TRIGGER authz_config_history_events
    AFTER INSERT ON authz_config_history
    FOR EACH ROW EXECUTE FUNCTION authz_config_events();
