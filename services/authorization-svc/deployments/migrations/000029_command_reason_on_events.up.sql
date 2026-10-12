-- 000029: the command's reason on every IAM event.
--
-- reason_code / purpose are required on destructive commands (Governance
-- Control Plane §16, AUTHZ_COMMAND_CONTRACT=enforce) and the store installs
-- them as app.reason on the transaction. The 000022 history records them for
-- roles, bundles, SoD and ABAC rules, but assignments and delegations have no
-- history table: their record is the iam.assignment.* / iam.delegation.*
-- event, and authz_emit_event left app.reason out — so the reason given for
-- revoking a grant or withdrawing a delegation was demanded and then dropped.
--
-- The envelope now carries it as command_reason (distinct from a delegation
-- payload's own `reason`, which is why it was lent). Null when none was given
-- (a consumer projection, or warn mode).

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
            'command_reason', NULLIF(current_setting('app.reason', true), ''),
            'payload',        p_payload)),
        p_tenant);
END;
$$;
