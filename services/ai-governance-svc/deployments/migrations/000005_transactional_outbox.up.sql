CREATE TABLE ai_governance_outbox (
    outbox_id       BIGSERIAL PRIMARY KEY,
    event_id        TEXT NOT NULL UNIQUE,
    event_type      TEXT NOT NULL,
    aggregate_type  TEXT NOT NULL,
    aggregate_id    TEXT NOT NULL,
    tenant_id       TEXT NOT NULL DEFAULT '',
    actor_id        TEXT NOT NULL DEFAULT '',
    correlation_id  TEXT NOT NULL DEFAULT '',
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    payload         JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    available_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_until     TIMESTAMPTZ,
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT
);

CREATE INDEX idx_ai_governance_outbox_ready
    ON ai_governance_outbox (available_at, outbox_id)
    WHERE published_at IS NULL;
CREATE INDEX idx_ai_governance_outbox_aggregate
    ON ai_governance_outbox (aggregate_type, aggregate_id, outbox_id)
    WHERE published_at IS NULL;

CREATE TABLE ai_executions (
    execution_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    use_case_id               UUID NOT NULL REFERENCES ai_use_cases(use_case_id),
    model_release_id          UUID NOT NULL REFERENCES ai_model_releases(model_release_id),
    package_id                TEXT NOT NULL,
    package_version           TEXT NOT NULL,
    idempotency_key           TEXT NOT NULL,
    request_sha256            CHAR(64) NOT NULL,
    status                    TEXT NOT NULL CHECK (status = 'BLOCKED'),
    block_reason              TEXT NOT NULL,
    blocked_by                JSONB NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT NOT NULL,
    UNIQUE (tenant_id, idempotency_key)
);

CREATE INDEX idx_ai_executions_tenant_created
    ON ai_executions (tenant_id, created_at DESC);
ALTER TABLE ai_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_executions_tenant_isolation_policy ON ai_executions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TABLE ai_incidents (
    incident_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    severity                  TEXT NOT NULL CHECK (severity IN ('AI-P0','AI-P1','AI-P2','AI-P3')),
    model_release_id          UUID NOT NULL REFERENCES ai_model_releases(model_release_id),
    description               TEXT NOT NULL,
    evidence_references       JSONB NOT NULL DEFAULT '[]'::jsonb,
    status                    TEXT NOT NULL DEFAULT 'OPEN' CHECK (status = 'OPEN'),
    idempotency_key           TEXT NOT NULL,
    request_sha256            CHAR(64) NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT NOT NULL,
    UNIQUE (tenant_id, idempotency_key)
);

CREATE INDEX idx_ai_incidents_tenant_created
    ON ai_incidents (tenant_id, created_at DESC);
ALTER TABLE ai_incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_incidents FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_incidents_tenant_isolation_policy ON ai_incidents
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE OR REPLACE FUNCTION aig_enqueue_outbox()
RETURNS trigger AS $$
DECLARE
    row_data JSONB := to_jsonb(NEW);
    event_type_value TEXT := TG_ARGV[0];
    event_id_value TEXT;
    aggregate_id_value TEXT;
    tenant_value TEXT;
    actor_value TEXT;
    event_payload JSONB;
BEGIN
    event_payload := row_data;
    IF TG_OP = 'UPDATE' AND TG_ARGV[1] <> ''
        AND (row_data ->> TG_ARGV[1]) IS NOT DISTINCT FROM (to_jsonb(OLD) ->> TG_ARGV[1]) THEN
        RETURN NEW;
    END IF;

    IF TG_OP = 'UPDATE' AND TG_NARGS > 2 AND TG_ARGV[2] <> '' THEN
        event_type_value := TG_ARGV[2];
    END IF;

    aggregate_id_value := COALESCE(
        row_data ->> TG_ARGV[3],
        row_data ->> 'ai_run_id',
        row_data ->> 'automation_policy_id',
        row_data ->> 'automation_action_id',
        row_data ->> 'provider_registration_id',
        row_data ->> 'policy_change_approval_id',
        row_data ->> 'use_case_id',
        row_data ->> 'model_release_id',
        row_data ->> 'action_type'
    );
    IF aggregate_id_value IS NULL THEN
        RAISE EXCEPTION 'cannot identify aggregate for outbox event on %', TG_TABLE_NAME;
    END IF;

    tenant_value := COALESCE(row_data ->> 'tenant_id', '');
    actor_value := COALESCE(
        NULLIF(current_setting('app.actor_id', true), ''),
        row_data ->> 'decided_by_principal_id',
        row_data ->> 'approved_by_principal_id',
        row_data ->> 'created_by_principal_id',
        row_data ->> 'proposed_by_principal_id',
        ''
    );
    event_id_value := 'evt-' || gen_random_uuid()::TEXT;
    IF TG_NARGS > 4 AND TG_ARGV[4] <> '' THEN
        event_payload := row_data - string_to_array(TG_ARGV[4], ',');
    END IF;

    INSERT INTO ai_governance_outbox (
        event_id, event_type, aggregate_type, aggregate_id,
        tenant_id, actor_id, correlation_id, payload
    ) VALUES (
        event_id_value, event_type_value, TG_TABLE_NAME, aggregate_id_value,
        tenant_value, actor_value,
        COALESCE(NULLIF(current_setting('app.correlation_id', true), ''), ''),
        event_payload
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public;

CREATE OR REPLACE FUNCTION aig_enqueue_model_release_outbox()
RETURNS trigger AS $$
DECLARE
    event_type_value TEXT;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.release_state IS NOT DISTINCT FROM OLD.release_state THEN
        RETURN NEW;
    END IF;

    IF TG_OP = 'INSERT' THEN
        event_type_value := 'ai.model.release_registered';
    ELSIF OLD.release_state = 'RESTRICTED' AND NEW.release_state = 'ACTIVE' THEN
        event_type_value := 'ai.model.release_unrestricted';
    ELSE
        event_type_value := CASE NEW.release_state
            WHEN 'DUE_DILIGENCE' THEN 'ai.model.release_due_diligence_recorded'
            WHEN 'EVALUATING' THEN 'ai.model.release_evaluated'
            WHEN 'APPROVED' THEN 'ai.model.release_approved'
            WHEN 'REJECTED' THEN 'ai.model.release_rejected'
            WHEN 'BLOCKED' THEN 'ai.model.release_blocked'
            WHEN 'ACTIVE' THEN 'ai.model.release_activated'
            WHEN 'RESTRICTED' THEN 'ai.model.release_restricted'
            WHEN 'QUARANTINED' THEN 'ai.model.release_quarantined'
            WHEN 'RETIRED' THEN 'ai.model.release_retired'
            ELSE NULL
        END;
    END IF;

    IF event_type_value IS NULL THEN
        RAISE EXCEPTION 'no outbox event mapping for model release state %', NEW.release_state;
    END IF;

    INSERT INTO ai_governance_outbox (
        event_id, event_type, aggregate_type, aggregate_id, actor_id, correlation_id, payload
    ) VALUES (
        'evt-' || gen_random_uuid()::TEXT,
        event_type_value,
        TG_TABLE_NAME,
        NEW.model_release_id::TEXT,
        COALESCE(NULLIF(current_setting('app.actor_id', true), ''), NEW.created_by_principal_id, ''),
        COALESCE(NULLIF(current_setting('app.correlation_id', true), ''), ''),
        to_jsonb(NEW)
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public;

CREATE TRIGGER aig_outbox_ai_runs
    AFTER INSERT ON ai_runs
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('ai_run.created', '', '', 'ai_run_id');

CREATE TRIGGER aig_outbox_action_risk_classifications
    AFTER INSERT OR UPDATE ON action_risk_classifications
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('action_risk_classification.set', '', '', 'action_type');

CREATE TRIGGER aig_outbox_automation_policies
    AFTER INSERT ON automation_policies
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('automation_policy.created', '', '', 'automation_policy_id');

CREATE TRIGGER aig_outbox_automation_actions_insert
    AFTER INSERT ON automation_actions
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('automation_action.proposed', '', '', 'automation_action_id');

CREATE TRIGGER aig_outbox_automation_actions_update
    AFTER UPDATE ON automation_actions
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('automation_action.decided', 'status', '', 'automation_action_id');

CREATE TRIGGER aig_outbox_model_provider_registrations
    AFTER INSERT OR UPDATE ON model_provider_registrations
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('model_provider.registered', '', '', 'provider_registration_id');

CREATE TRIGGER aig_outbox_policy_change_approvals_insert
    AFTER INSERT ON policy_change_approvals
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('policy_change.proposed', '', '', 'policy_change_approval_id');

CREATE TRIGGER aig_outbox_policy_change_approvals_update
    AFTER UPDATE ON policy_change_approvals
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('policy_change.decided', 'decision', '', 'policy_change_approval_id');

CREATE TRIGGER aig_outbox_use_cases_insert
    AFTER INSERT ON ai_use_cases
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('ai.use_case.state_changed', '', '', 'use_case_id');

CREATE TRIGGER aig_outbox_use_cases_update
    AFTER UPDATE ON ai_use_cases
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox('ai.use_case.state_changed', 'lifecycle_state', '', 'use_case_id');

CREATE TRIGGER aig_outbox_model_releases
    AFTER INSERT OR UPDATE ON ai_model_releases
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_model_release_outbox();

CREATE TRIGGER aig_outbox_ai_executions
    AFTER INSERT ON ai_executions
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox(
        'ai.execution.blocked', '', '', 'execution_id', 'idempotency_key,request_sha256'
    );

CREATE TRIGGER aig_outbox_ai_incidents
    AFTER INSERT ON ai_incidents
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox(
        'ai.incident.opened', '', '', 'incident_id',
        'description,evidence_references,idempotency_key,request_sha256'
    );
