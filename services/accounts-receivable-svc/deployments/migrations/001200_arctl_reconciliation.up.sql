-- Migration: 001200_arctl_reconciliation.up.sql
-- AR-to-GL control module (spec ZS-SVC-C-001 invariants 15 and 17, section 15 ARControlLink, section 17, #21 #35 #36).
--   arctl_runs             one control run per (tenant, legal entity, as-of); append-only, a rerun is a NEW row linked by supersedes
--   arctl_findings         every break found by a run; append-only
--   arctl_evidence         the exact inputs of the run (documents, open items, posting facts, GL read, tolerance policy reference)
--   arctl_acknowledgements annotations on findings; they never edit or clear the finding

CREATE TABLE IF NOT EXISTS arctl_runs (
    run_id                   UUID PRIMARY KEY,
    tenant_id                UUID         NOT NULL,
    legal_entity_id          UUID         NOT NULL,
    as_of_date               DATE         NOT NULL,
    cutoff_date              DATE         NOT NULL,
    result                   VARCHAR(12)  NOT NULL CHECK (result IN ('PASS','BREAK','UNVERIFIED')),
    supersedes_run_id        UUID         REFERENCES arctl_runs (run_id),
    tolerance_policy_id      VARCHAR(255) NOT NULL DEFAULT '',
    tolerance_policy_version VARCHAR(64)  NOT NULL DEFAULT '',
    reason_code              VARCHAR(128) NOT NULL DEFAULT '',
    population_watermark     VARCHAR(255) NOT NULL DEFAULT '',
    gl_watermark             VARCHAR(255) NOT NULL DEFAULT '',
    inputs_hash              VARCHAR(64)  NOT NULL,
    summary                  JSONB        NOT NULL,
    run_by                   VARCHAR(255) NOT NULL,
    correlation_id           VARCHAR(255) NOT NULL DEFAULT '',
    computed_at              TIMESTAMPTZ  NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    version                  INTEGER      NOT NULL DEFAULT 1,
    CONSTRAINT arctl_run_not_self_superseding CHECK (supersedes_run_id IS NULL OR supersedes_run_id <> run_id)
);
-- exactly one first run per (entity, as-of), and a run can be superseded at most once (a linear chain)
CREATE UNIQUE INDEX IF NOT EXISTS uq_arctl_first_run ON arctl_runs (tenant_id, legal_entity_id, as_of_date) WHERE supersedes_run_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_arctl_superseded_once ON arctl_runs (supersedes_run_id) WHERE supersedes_run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_arctl_runs_scope ON arctl_runs (tenant_id, legal_entity_id, as_of_date, created_at DESC);

CREATE TABLE IF NOT EXISTS arctl_findings (
    finding_id      UUID PRIMARY KEY,
    run_id          UUID          NOT NULL REFERENCES arctl_runs (run_id),
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    seq             INTEGER       NOT NULL CHECK (seq >= 1),
    category        VARCHAR(32)   NOT NULL CHECK (category IN ('POPULATION_COMPLETENESS','ACCOUNTING_EVENT','GL_TIEOUT','EXCEPTION_ROLLUP')),
    kind            VARCHAR(48)   NOT NULL,
    code            VARCHAR(64)   NOT NULL,
    severity        VARCHAR(8)    NOT NULL CHECK (severity IN ('INFO','WARNING','BREAK')),
    subject_type    VARCHAR(32)   NOT NULL DEFAULT '',
    subject_id      VARCHAR(255)  NOT NULL DEFAULT '',
    currency        VARCHAR(3)    NOT NULL DEFAULT '',
    ar_amount       NUMERIC(20,4),
    gl_amount       NUMERIC(20,4),
    difference      NUMERIC(20,4),
    detail          JSONB         NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_arctl_findings_run ON arctl_findings (tenant_id, run_id, seq);

CREATE TABLE IF NOT EXISTS arctl_evidence (
    evidence_id     UUID PRIMARY KEY,
    run_id          UUID          NOT NULL REFERENCES arctl_runs (run_id),
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    kind            VARCHAR(32)   NOT NULL,
    content         JSONB         NOT NULL,
    content_hash    VARCHAR(64)   NOT NULL,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, kind)
);

CREATE TABLE IF NOT EXISTS arctl_acknowledgements (
    ack_id          UUID PRIMARY KEY,
    finding_id      UUID          NOT NULL REFERENCES arctl_findings (finding_id),
    run_id          UUID          NOT NULL REFERENCES arctl_runs (run_id),
    tenant_id       UUID          NOT NULL,
    legal_entity_id UUID          NOT NULL,
    reason_code     VARCHAR(128)  NOT NULL CHECK (length(btrim(reason_code)) > 0),
    evidence_id     VARCHAR(255)  NOT NULL CHECK (length(btrim(evidence_id)) > 0),
    note            TEXT          NOT NULL DEFAULT '',
    acknowledged_by VARCHAR(255)  NOT NULL,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_arctl_acks_finding ON arctl_acknowledgements (tenant_id, finding_id);

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['arctl_runs','arctl_findings','arctl_evidence','arctl_acknowledgements'] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t || '_append_only', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation()', t || '_append_only', t);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
