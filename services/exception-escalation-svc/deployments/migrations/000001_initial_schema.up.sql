-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS exception_cases (
    exception_case_id      TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    legal_entity_id        TEXT        NOT NULL,
    jurisdiction_id        TEXT        NOT NULL,
    exception_type         TEXT        NOT NULL,
    severity_level         TEXT        NOT NULL DEFAULT 'MEDIUM',
    linked_object_type     TEXT        NOT NULL,
    linked_object_id       TEXT        NOT NULL,
    description            TEXT        NOT NULL DEFAULT '',
    case_status            TEXT        NOT NULL DEFAULT 'OPEN',
    assigned_to_role       TEXT        NOT NULL DEFAULT '',
    assigned_to_user       TEXT        NOT NULL DEFAULT '',
    escalated_at           TIMESTAMPTZ,
    closed_at              TIMESTAMPTZ,
    closed_by              TEXT        NOT NULL DEFAULT '',
    closure_reason         TEXT        NOT NULL DEFAULT '',
    created_by             TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exception_case_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS escalation_records (
    escalation_record_id   TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    exception_case_id      TEXT        NOT NULL,
    escalated_to_role      TEXT        NOT NULL,
    escalated_to_user      TEXT        NOT NULL DEFAULT '',
    escalation_reason      TEXT        NOT NULL,
    escalation_status      TEXT        NOT NULL DEFAULT 'PENDING',
    escalated_by           TEXT        NOT NULL,
    escalated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at            TIMESTAMPTZ,
    response_notes         TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (escalation_record_id, tenant_id)
);

-- Row-Level Security: all queries must carry app.tenant_id
ALTER TABLE exception_cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE escalation_records ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS excase_tenant_isolation ON exception_cases;
CREATE POLICY excase_tenant_isolation ON exception_cases
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS escrec_tenant_isolation ON escalation_records;
CREATE POLICY escrec_tenant_isolation ON escalation_records
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_excase_tenant_entity ON exception_cases (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_excase_severity      ON exception_cases (severity_level);
CREATE INDEX IF NOT EXISTS idx_excase_status        ON exception_cases (case_status);
CREATE INDEX IF NOT EXISTS idx_excase_linked_object ON exception_cases (linked_object_type, linked_object_id);

CREATE INDEX IF NOT EXISTS idx_escrec_case_id       ON escalation_records (tenant_id, exception_case_id);
CREATE INDEX IF NOT EXISTS idx_escrec_role          ON escalation_records (escalated_to_role);

COMMIT;
