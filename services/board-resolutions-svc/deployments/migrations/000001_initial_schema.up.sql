-- +migrate Up
BEGIN;

CREATE TABLE IF NOT EXISTS board_meetings (
    meeting_id      TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    legal_entity_id TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    scheduled_at    TIMESTAMPTZ NOT NULL,
    location        TEXT,
    status          TEXT        NOT NULL DEFAULT 'SCHEDULED',
    minutes_summary TEXT,
    effective_from  DATE        NOT NULL,
    effective_to    DATE,
    created_by      TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (meeting_id, tenant_id)
);

CREATE TABLE IF NOT EXISTS board_resolutions (
    resolution_id     TEXT        NOT NULL,
    meeting_id        TEXT        NOT NULL,
    tenant_id         TEXT        NOT NULL,
    legal_entity_id   TEXT        NOT NULL,
    resolution_number TEXT        NOT NULL,
    title             TEXT        NOT NULL,
    content           TEXT        NOT NULL,
    category          TEXT        NOT NULL,
    status            TEXT        NOT NULL DEFAULT 'PROPOSED',
    votes_for         INTEGER     NOT NULL DEFAULT 0,
    votes_against     INTEGER     NOT NULL DEFAULT 0,
    abstentions       INTEGER     NOT NULL DEFAULT 0,
    passed_at         TIMESTAMPTZ,
    passed_by         TEXT,
    document_vault_id TEXT,
    effective_from    DATE        NOT NULL,
    effective_to      DATE,
    created_by        TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (resolution_id, tenant_id)
);

-- Row-Level Security
ALTER TABLE board_meetings ENABLE ROW LEVEL SECURITY;
ALTER TABLE board_resolutions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS meetings_tenant_isolation ON board_meetings;
CREATE POLICY meetings_tenant_isolation ON board_meetings
    USING (tenant_id = current_setting('app.tenant_id', true));

DROP POLICY IF EXISTS resolutions_tenant_isolation ON board_resolutions;
CREATE POLICY resolutions_tenant_isolation ON board_resolutions
    USING (tenant_id = current_setting('app.tenant_id', true));

-- Indexes
CREATE INDEX IF NOT EXISTS idx_meetings_tenant_entity ON board_meetings (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_resolutions_tenant_entity ON board_resolutions (tenant_id, legal_entity_id);
CREATE INDEX IF NOT EXISTS idx_resolutions_meeting_id ON board_resolutions (meeting_id);
CREATE INDEX IF NOT EXISTS idx_resolutions_status ON board_resolutions (status);

COMMIT;
