-- 20260818001600_board_resolutions_svc.sql
-- board-resolutions-svc → schema `board_resolutions`
--
-- Squashed end state of 000001_initial_schema and
-- 000002_force_rls_and_constraints. Two tables: board_meetings,
-- board_resolutions.
--
-- The CHECK constraints are VALID here. 000002 added them NOT VALID because
-- board minutes and resolutions are the record of what a board decided, and a
-- migration that rewrites them to fit a rule written afterwards is worse than
-- one that constrains everything from now on. That protects an existing
-- backlog; this database has none.
--
-- Both tables keep their COMPOSITE primary keys (id, tenant_id) from 000001 —
-- unusual across this migration set, and worth keeping: it makes a row's tenant
-- part of its identity, so a resolution cannot be re-pointed at another tenant
-- by an UPDATE.

CREATE SCHEMA IF NOT EXISTS board_resolutions;

COMMENT ON SCHEMA board_resolutions IS
    'board-resolutions-svc. Board meetings and the resolutions passed at them.';

GRANT USAGE ON SCHEMA board_resolutions TO zoiko_backend, authenticated;

-- ── board_meetings ───────────────────────────────────────────────────────────

CREATE TABLE board_resolutions.board_meetings (
    meeting_id      TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL,
    legal_entity_id TEXT        NOT NULL,

    title           TEXT        NOT NULL,
    scheduled_at    TIMESTAMPTZ NOT NULL,
    location        TEXT,

    -- SCHEDULED | IN_PROGRESS | ADJOURNED | CANCELLED
    status          TEXT        NOT NULL DEFAULT 'SCHEDULED',
    minutes_summary TEXT,

    effective_from  DATE        NOT NULL,
    effective_to    DATE,

    -- Attribution is the verified caller. The handler additionally refuses a
    -- created_by that disagrees with the principal (requireSelfAttribution) —
    -- this service is the only one on the estate that does, and the default
    -- here is the database-side half of the same rule.
    created_by      TEXT        NOT NULL DEFAULT app.current_principal_id(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (meeting_id, tenant_id),

    CONSTRAINT board_meetings_status_known
        CHECK (status IN ('SCHEDULED', 'IN_PROGRESS', 'ADJOURNED', 'CANCELLED')),

    CONSTRAINT board_meetings_period_ordered
        CHECK (effective_to IS NULL OR effective_to >= effective_from)
);

CREATE INDEX idx_meetings_tenant_entity
    ON board_resolutions.board_meetings (tenant_id, legal_entity_id);
CREATE INDEX idx_meetings_tenant_scheduled
    ON board_resolutions.board_meetings (tenant_id, scheduled_at DESC, meeting_id DESC);

-- ── board_resolutions ────────────────────────────────────────────────────────

CREATE TABLE board_resolutions.board_resolutions (
    resolution_id     TEXT        NOT NULL,
    meeting_id        TEXT        NOT NULL,
    tenant_id         TEXT        NOT NULL,
    legal_entity_id   TEXT        NOT NULL,

    resolution_number TEXT        NOT NULL,
    title             TEXT        NOT NULL,
    content           TEXT        NOT NULL,

    -- GOVERNANCE | FINANCIAL | OPERATIONAL | EXECUTIVE | STATUTORY.
    -- This is sent to evidence-requirements-svc as the domain_code, so an
    -- unrecognised value asks the catalogue about a domain that does not exist
    -- and comes back with no requirements — an evidence gate bypassed by a typo.
    category          TEXT        NOT NULL,

    -- PROPOSED | PASSED | REJECTED | RESCINDED
    status            TEXT        NOT NULL DEFAULT 'PROPOSED',

    votes_for         INTEGER     NOT NULL DEFAULT 0,
    votes_against     INTEGER     NOT NULL DEFAULT 0,
    abstentions       INTEGER     NOT NULL DEFAULT 0,

    passed_at         TIMESTAMPTZ,
    passed_by         TEXT,

    document_vault_id TEXT,

    effective_from    DATE        NOT NULL,
    effective_to      DATE,

    created_by        TEXT        NOT NULL DEFAULT app.current_principal_id(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (resolution_id, tenant_id),

    -- Vote tallies are counts of people. Nothing stopped a negative one being
    -- stored, so a resolution could carry -5 votes against and its arithmetic
    -- would still "work".
    CONSTRAINT board_resolutions_votes_non_negative
        CHECK (votes_for >= 0 AND votes_against >= 0 AND abstentions >= 0),

    CONSTRAINT board_resolutions_status_known
        CHECK (status IN ('PROPOSED', 'PASSED', 'REJECTED', 'RESCINDED')),

    CONSTRAINT board_resolutions_category_known
        CHECK (category IN ('GOVERNANCE', 'FINANCIAL', 'OPERATIONAL', 'EXECUTIVE', 'STATUTORY')),

    -- A PASSED resolution is the record that the board put something into
    -- force. Both halves must be present: passed_at without passed_by is an
    -- unattributed finalisation, which is exactly what segregation of duties
    -- exists to prevent.
    CONSTRAINT board_resolutions_passed_is_attributed
        CHECK (status <> 'PASSED' OR (passed_by IS NOT NULL AND passed_at IS NOT NULL)),

    CONSTRAINT board_resolutions_period_ordered
        CHECK (effective_to IS NULL OR effective_to >= effective_from),

    -- A resolution belongs to a meeting in the same tenant. The composite
    -- primary key on board_meetings makes this expressible; 000001 had no
    -- foreign key at all, so a resolution could name a meeting that did not
    -- exist, or one belonging to someone else.
    CONSTRAINT board_resolutions_meeting_fk
        FOREIGN KEY (meeting_id, tenant_id)
        REFERENCES board_resolutions.board_meetings (meeting_id, tenant_id)
);

CREATE INDEX idx_resolutions_tenant_entity
    ON board_resolutions.board_resolutions (tenant_id, legal_entity_id);
CREATE INDEX idx_resolutions_meeting_id
    ON board_resolutions.board_resolutions (meeting_id);
CREATE INDEX idx_resolutions_status
    ON board_resolutions.board_resolutions (status);
CREATE INDEX idx_resolutions_tenant_meeting
    ON board_resolutions.board_resolutions (tenant_id, meeting_id);
CREATE INDEX idx_resolutions_tenant_created
    ON board_resolutions.board_resolutions (tenant_id, created_at DESC, resolution_id DESC);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE board_resolutions.board_meetings    ENABLE ROW LEVEL SECURITY;
ALTER TABLE board_resolutions.board_meetings    FORCE  ROW LEVEL SECURITY;
ALTER TABLE board_resolutions.board_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE board_resolutions.board_resolutions FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON board_resolutions.board_meetings
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON board_resolutions.board_meetings
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON board_resolutions.board_resolutions
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON board_resolutions.board_resolutions
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON board_resolutions.board_meetings    TO authenticated;
GRANT SELECT ON board_resolutions.board_resolutions TO authenticated;

-- Both transition (a meeting is adjourned, a resolution voted on and passed),
-- so UPDATE is granted. Neither is ever deleted — a rescinded resolution is
-- RESCINDED, not removed.
GRANT SELECT, INSERT, UPDATE ON board_resolutions.board_meetings    TO zoiko_backend;
GRANT SELECT, INSERT, UPDATE ON board_resolutions.board_resolutions TO zoiko_backend;
