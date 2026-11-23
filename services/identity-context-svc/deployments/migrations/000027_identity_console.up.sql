-- Migration: 000019_identity_console.up.sql
--
-- IC-2 / IC-3 follow-up to 000017 and 000018.
--
-- 1. authentication_events and principal_assurance_history are written by the
--    service in the same transaction as the access_decision_log row they
--    mirror (store.projectDecision), so the app role needs INSERT; 000017
--    granted SELECT only. Both stay append-only: a trigger refuses UPDATE and
--    DELETE for every role, the owner included.
-- 2. identity_merges is the alias table of ORG-1 MergeDuplicateIdentity: a
--    retired id resolves to its survivor. A retired id is merged once, and the
--    merge records its reason. Also append-only.
-- 3. identity_links (000018) is dropped. Nothing ever wrote it; an external
--    IdP subject is bound to a principal in federation_links (INT-10, 000012)
--    and federated_identities (auth plane, 000014), which
--    GET /v1/subjects/{id}/links now reads.
--
-- Grants are guarded on the role, so a database without create-app-roles.sh
-- applied still migrates.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_identity_context') THEN
        GRANT SELECT, INSERT ON authentication_events, principal_assurance_history TO app_identity_context;
        GRANT SELECT, INSERT ON identity_merges TO app_identity_context;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION identity_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END
$$;

CREATE TRIGGER authentication_events_append_only
    BEFORE UPDATE OR DELETE ON authentication_events
    FOR EACH ROW EXECUTE FUNCTION identity_append_only();
CREATE TRIGGER principal_assurance_history_append_only
    BEFORE UPDATE OR DELETE ON principal_assurance_history
    FOR EACH ROW EXECUTE FUNCTION identity_append_only();
CREATE TRIGGER identity_merges_append_only
    BEFORE UPDATE OR DELETE ON identity_merges
    FOR EACH ROW EXECUTE FUNCTION identity_append_only();

ALTER TABLE identity_merges ADD COLUMN reason TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX uq_identity_merges_retired ON identity_merges (tenant_id, retired_id);
CREATE INDEX idx_identity_merges_survivor ON identity_merges (tenant_id, survivor_id);

DROP TABLE identity_links;

-- Console list orderings.
CREATE INDEX IF NOT EXISTS idx_session_contexts_tenant_issued
    ON session_contexts (tenant_id, issued_at DESC, session_context_id DESC);
CREATE INDEX IF NOT EXISTS idx_principals_tenant_created
    ON principals (tenant_id, created_at DESC, principal_id DESC);
