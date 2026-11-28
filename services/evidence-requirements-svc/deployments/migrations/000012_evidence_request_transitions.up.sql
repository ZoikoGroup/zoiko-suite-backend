-- Migration 000012: append-only history of every PBC evidence request
-- (ZS-STATE ST-2; tracker D2-7 ER-S6, 23 Nov 2026).
--
-- A trigger on evidence_requests writes one row per creation and per change of
-- status, assignee or due date, so no command path can move a request without
-- leaving its history: the row records from/to status, the command and the
-- actor (from the transaction-local app.command / app.actor_id the service
-- sets), the record_version the change produced and the correlation id.
-- Rows are never updated or deleted.

CREATE TABLE evidence_request_transitions (
    transition_id   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID        NOT NULL,
    request_id      UUID        NOT NULL REFERENCES evidence_requests (request_id),
    from_status     TEXT,
    to_status       TEXT        NOT NULL,
    command         TEXT        NOT NULL,
    actor           TEXT        NOT NULL,
    changed_fields  TEXT[]      NOT NULL,
    assigned_to_principal_id TEXT,
    due_at          TIMESTAMPTZ,
    record_version  BIGINT      NOT NULL,
    correlation_id  TEXT,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_evidence_request_transitions_request ON evidence_request_transitions (tenant_id, request_id, recorded_at, transition_id);

ALTER TABLE evidence_request_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE evidence_request_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON evidence_request_transitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID);

CREATE OR REPLACE FUNCTION evidence_request_record_transition() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    fields TEXT[] := '{}';
BEGIN
    IF TG_OP = 'INSERT' THEN
        fields := ARRAY['status', 'assigned_to_principal_id', 'due_at'];
    ELSE
        IF NEW.status IS DISTINCT FROM OLD.status THEN fields := fields || 'status'::text; END IF;
        IF NEW.assigned_to_principal_id IS DISTINCT FROM OLD.assigned_to_principal_id THEN fields := fields || 'assigned_to_principal_id'::text; END IF;
        IF NEW.due_at IS DISTINCT FROM OLD.due_at THEN fields := fields || 'due_at'::text; END IF;
        IF cardinality(fields) = 0 THEN
            RETURN NEW;
        END IF;
    END IF;
    INSERT INTO evidence_request_transitions (tenant_id, request_id, from_status, to_status, command, actor, changed_fields,
        assigned_to_principal_id, due_at, record_version, correlation_id)
    VALUES (NEW.tenant_id, NEW.request_id, CASE WHEN TG_OP = 'INSERT' THEN NULL ELSE OLD.status END, NEW.status,
        COALESCE(NULLIF(current_setting('app.command', true), ''), CASE WHEN TG_OP = 'INSERT' THEN 'create' ELSE 'unknown' END),
        COALESCE(NULLIF(current_setting('app.actor_id', true), ''), CASE WHEN TG_OP = 'INSERT' THEN NEW.created_by_principal_id ELSE 'unknown' END),
        fields, NEW.assigned_to_principal_id, NEW.due_at, NEW.record_version, NULLIF(current_setting('app.correlation_id', true), ''));
    RETURN NEW;
END $$;

CREATE TRIGGER evidence_request_record_transition
    AFTER INSERT OR UPDATE ON evidence_requests
    FOR EACH ROW EXECUTE FUNCTION evidence_request_record_transition();

CREATE OR REPLACE FUNCTION evidence_request_transitions_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'evidence_request_transitions is append-only' USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER evidence_request_transitions_append_only
    BEFORE UPDATE OR DELETE ON evidence_request_transitions
    FOR EACH ROW EXECUTE FUNCTION evidence_request_transitions_append_only();

-- History of requests created before this migration starts at their state now.
INSERT INTO evidence_request_transitions (tenant_id, request_id, from_status, to_status, command, actor, changed_fields,
    assigned_to_principal_id, due_at, record_version, correlation_id)
SELECT tenant_id, request_id, NULL, status, 'backfill:000012', created_by_principal_id, ARRAY['status'],
       assigned_to_principal_id, due_at, record_version, correlation_id
  FROM evidence_requests;
