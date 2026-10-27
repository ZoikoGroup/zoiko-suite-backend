-- AIG-04 — ZS-SVC-X-001 §7: Human Oversight, Output Disposition & Decision
-- Boundary. Every non-O0 AI output passes through a disposition record
-- before any downstream authority may rely on it. BLOCKED belongs to
-- AIG-03's upstream validation (not created here); SUPERSEDED requires a
-- replacement-output linking workflow not implemented in this migration.
CREATE TABLE ai_output_dispositions (
    disposition_id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    ai_run_id                UUID NOT NULL REFERENCES ai_runs(ai_run_id),
    oversight_class          TEXT NOT NULL CHECK (oversight_class IN ('O0','O1','O2','O3','O4')),
    status                   TEXT NOT NULL CHECK (status IN ('DRAFT_ASSISTIVE','REVIEW_REQUIRED','ACCEPTED','REJECTED')),
    reason                   TEXT,
    idempotency_key          TEXT NOT NULL,
    request_sha256           CHAR(64) NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id   TEXT NOT NULL,
    decided_at                TIMESTAMPTZ,
    decided_by_principal_id   TEXT,
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (ai_run_id)
);

CREATE INDEX idx_ai_output_dispositions_tenant_created
    ON ai_output_dispositions (tenant_id, created_at DESC);

ALTER TABLE ai_output_dispositions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_output_dispositions FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_output_dispositions_tenant_isolation_policy ON ai_output_dispositions
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER aig_outbox_ai_output_dispositions_insert
    AFTER INSERT ON ai_output_dispositions
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox(
        'ai.output_disposition.created', '', '', 'disposition_id', 'idempotency_key,request_sha256'
    );

CREATE TRIGGER aig_outbox_ai_output_dispositions_update
    AFTER UPDATE ON ai_output_dispositions
    FOR EACH ROW EXECUTE FUNCTION aig_enqueue_outbox(
        'ai.output_disposition.decided', 'status', '', 'disposition_id', 'idempotency_key,request_sha256'
    );
