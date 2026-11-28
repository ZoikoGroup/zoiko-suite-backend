-- Migration: 000021_signatory_binding.up.sql
--
-- ZS-GOV-INT-002 §4.2 signature handoff. A workflow created from a policy-svc
-- SIGNATORY_MATRIX decision binds that decision: which requirement (distinct
-- signers, corporate offices, sequencing) its QUORUM stage must meet, and the
-- exact document hash every signature is bound to. The decision itself stays
-- in policy-svc (signed); this row pins its id, version and the requirement as
-- verified when the workflow was created.
--
-- Each signature (an APPROVE quorum vote) records the offices Authorization
-- confirmed the signer holds at that moment and the document hash signed.
CREATE TABLE IF NOT EXISTS workflow_signatory_bindings (
    workflow_instance_id         UUID        PRIMARY KEY REFERENCES workflow_instances (workflow_instance_id),
    tenant_id                    UUID        NOT NULL,
    signatory_decision_id        UUID        NOT NULL,
    signatory_policy_version_id  UUID        NOT NULL,
    signatory_rule_id            TEXT        NOT NULL,
    document_hash                TEXT        NOT NULL CHECK (document_hash ~ '^sha256:[0-9a-f]{64}$'),
    requirement                  JSONB       NOT NULL,
    bound_at                     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE workflow_signatory_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_signatory_bindings FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS workflow_signatory_bindings_tenant_isolation ON workflow_signatory_bindings;
CREATE POLICY workflow_signatory_bindings_tenant_isolation ON workflow_signatory_bindings
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- A binding is evidence: never changed, never removed.
CREATE OR REPLACE FUNCTION workflow_signatory_bindings_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workflow_signatory_bindings is append-only';
END $$;
DROP TRIGGER IF EXISTS trg_workflow_signatory_bindings_immutable ON workflow_signatory_bindings;
CREATE TRIGGER trg_workflow_signatory_bindings_immutable
    BEFORE UPDATE OR DELETE ON workflow_signatory_bindings
    FOR EACH ROW EXECUTE FUNCTION workflow_signatory_bindings_immutable();

ALTER TABLE workflow_stage_quorum_decisions
    ADD COLUMN IF NOT EXISTS signer_offices TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS document_hash  TEXT;
