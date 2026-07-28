-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS compliance_status_records (
    status_id              TEXT         NOT NULL,
    tenant_id              TEXT         NOT NULL,
    legal_entity_id        TEXT         NOT NULL,
    jurisdiction_id        TEXT         NOT NULL,
    domain_name            TEXT         NOT NULL DEFAULT 'OVERALL',
    overall_status         TEXT         NOT NULL DEFAULT 'COMPLIANT',
    health_score           NUMERIC(5,2) NOT NULL DEFAULT 100.00,
    total_obligations      INTEGER      NOT NULL DEFAULT 0,
    fulfilled_obligations  INTEGER      NOT NULL DEFAULT 0,
    pending_obligations    INTEGER      NOT NULL DEFAULT 0,
    overdue_obligations    INTEGER      NOT NULL DEFAULT 0,
    open_exceptions        INTEGER      NOT NULL DEFAULT 0,
    last_evaluated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    notes                  TEXT         NOT NULL DEFAULT '',
    effective_from         DATE         NOT NULL,
    effective_to           DATE,
    created_by             TEXT         NOT NULL,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (status_id, tenant_id),
    CONSTRAINT uk_compliance_status_entity_domain UNIQUE (tenant_id, legal_entity_id, jurisdiction_id, domain_name)
);

CREATE TABLE IF NOT EXISTS compliance_gaps (
    gap_id                 TEXT        NOT NULL,
    tenant_id              TEXT        NOT NULL,
    legal_entity_id        TEXT        NOT NULL,
    jurisdiction_id        TEXT        NOT NULL,
    domain_name            TEXT        NOT NULL,
    gap_type               TEXT        NOT NULL,
    severity               TEXT        NOT NULL DEFAULT 'MEDIUM',
    source_reference       TEXT        NOT NULL DEFAULT '',
    description            TEXT        NOT NULL DEFAULT '',
    remediation_plan       TEXT        NOT NULL DEFAULT '',
    status                 TEXT        NOT NULL DEFAULT 'OPEN',
    detected_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at            TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (gap_id, tenant_id)
);

-- Row-Level Security: all queries must carry app.tenant_id
ALTER TABLE compliance_status_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE compliance_gaps ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS comp_status_tenant_isolation ON compliance_status_records;
CREATE POLICY comp_status_tenant_isolation ON compliance_status_records
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS comp_gaps_tenant_isolation ON compliance_gaps;
CREATE POLICY comp_gaps_tenant_isolation ON compliance_gaps
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_comp_status_tenant_entity ON compliance_status_records (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_comp_status_jurisdiction   ON compliance_status_records (jurisdiction_id);
CREATE INDEX IF NOT EXISTS idx_comp_status_domain         ON compliance_status_records (domain_name);

CREATE INDEX IF NOT EXISTS idx_comp_gaps_tenant_entity   ON compliance_gaps (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_comp_gaps_severity        ON compliance_gaps (severity);
CREATE INDEX IF NOT EXISTS idx_comp_gaps_status          ON compliance_gaps (status);

COMMIT;
