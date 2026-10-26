-- 000012_completion.up.sql
-- Obligations Service — Group 2 completion pass (21 Oct 2026).
--
--  1. Outbox relay: retry with backoff. 000010 parked an event as FAILED after
--     five attempts, and the relay polls every 500 ms, so a broker outage of
--     about 2.5 seconds lost every pending event for good.
--  2. Overdue sweep: the scheduler lists tenants with due obligations, which
--     FORCE RLS hid from the runtime role (no app.tenant_id), so under the
--     real role nothing ever became OVERDUE. A SELECT-only policy gated on the
--     transaction-local app.overdue_sweep flag, the same shape as 000011.
--  3. Applicability decisions: Idempotency-Key semantics (same key + same
--     body replays, a changed body is refused).
--  4. Filing requirements: a status lifecycle with an append-only history.
--  5. Doc 03 §8.5 "Owns obligation definitions … obligation-to-entity
--     mappings": a versioned definition catalogue, entity mappings, and an
--     obligation pinned to the definition version it was raised from.

BEGIN;

-- 1 ── outbox backoff ──────────────────────────────────────────────────────
ALTER TABLE event_outbox ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE event_outbox ADD COLUMN IF NOT EXISTS failed_at TIMESTAMPTZ;
-- Rows parked by the old five-attempt rule were never delivered.
UPDATE event_outbox SET status = 'PENDING', next_attempt_at = NOW() WHERE status = 'FAILED';
CREATE INDEX IF NOT EXISTS idx_event_outbox_due
    ON event_outbox (next_attempt_at, created_at) WHERE status = 'PENDING';

-- 2 ── overdue sweep ───────────────────────────────────────────────────────
DROP POLICY IF EXISTS overdue_sweep_select ON obligations;
CREATE POLICY overdue_sweep_select ON obligations
    FOR SELECT
    USING (current_setting('app.overdue_sweep', true) = 'true');

-- 3 ── applicability idempotency ───────────────────────────────────────────
ALTER TABLE applicability_decisions ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE applicability_decisions ADD COLUMN IF NOT EXISTS request_hash TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS uq_applicability_decisions_idempotency
    ON applicability_decisions (tenant_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- 4 ── filing status lifecycle ─────────────────────────────────────────────
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE filing_requirements ADD COLUMN IF NOT EXISTS row_version INTEGER NOT NULL DEFAULT 1;
ALTER TABLE filing_requirements DROP CONSTRAINT IF EXISTS chk_filing_requirements_status;
ALTER TABLE filing_requirements ADD CONSTRAINT chk_filing_requirements_status
    CHECK (filing_status IN ('PENDING', 'IN_PREPARATION', 'SUBMITTED', 'ACCEPTED', 'REJECTED', 'WITHDRAWN'))
    NOT VALID;

CREATE TABLE IF NOT EXISTS filing_requirement_transitions (
    transition_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID        NOT NULL,
    filing_requirement_id  UUID        NOT NULL REFERENCES filing_requirements(filing_requirement_id),
    from_status            VARCHAR(32) NOT NULL,
    to_status              VARCHAR(32) NOT NULL,
    reason                 TEXT,
    actor_principal_id     TEXT        NOT NULL,
    correlation_id         TEXT        NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_filing_transitions_requirement
    ON filing_requirement_transitions (tenant_id, filing_requirement_id, created_at);

-- 5 ── definitions, versions, entity mappings ──────────────────────────────
CREATE TABLE IF NOT EXISTS obligation_definitions (
    obligation_definition_id UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID         NOT NULL,
    definition_code          VARCHAR(128) NOT NULL,
    current_version          INTEGER      NOT NULL DEFAULT 1,
    definition_status        VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE'
        CHECK (definition_status IN ('ACTIVE', 'RETIRED')),
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL,
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    retired_at               TIMESTAMPTZ,
    CONSTRAINT uq_obligation_definitions_code UNIQUE (tenant_id, definition_code)
);

CREATE TABLE IF NOT EXISTS obligation_definition_versions (
    obligation_definition_id UUID         NOT NULL REFERENCES obligation_definitions(obligation_definition_id),
    version                  INTEGER      NOT NULL CHECK (version >= 1),
    tenant_id                UUID         NOT NULL,
    jurisdiction_id          UUID         NOT NULL,
    obligation_type          VARCHAR(64)  NOT NULL,
    obligation_source_type   VARCHAR(64)  NOT NULL,
    obligation_source_id     TEXT         NOT NULL,
    source_reference         TEXT         NOT NULL,
    severity_level           VARCHAR(32)  NOT NULL,
    responsible_function     TEXT         NOT NULL,
    description              TEXT         NOT NULL DEFAULT '',
    change_reason            TEXT         NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT         NOT NULL,
    PRIMARY KEY (obligation_definition_id, version)
);

CREATE TABLE IF NOT EXISTS obligation_entity_mappings (
    mapping_id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID        NOT NULL,
    obligation_definition_id UUID        NOT NULL REFERENCES obligation_definitions(obligation_definition_id),
    legal_entity_id          UUID        NOT NULL,
    effective_from           TIMESTAMPTZ NOT NULL,
    effective_to             TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id  TEXT        NOT NULL,
    ended_by_principal_id    TEXT,
    CONSTRAINT chk_mapping_interval CHECK (effective_to IS NULL OR effective_to > effective_from)
);
-- One open mapping per (definition, entity).
CREATE UNIQUE INDEX IF NOT EXISTS uq_obligation_entity_mappings_open
    ON obligation_entity_mappings (tenant_id, obligation_definition_id, legal_entity_id)
    WHERE effective_to IS NULL;

ALTER TABLE obligations ADD COLUMN IF NOT EXISTS obligation_definition_id UUID
    REFERENCES obligation_definitions(obligation_definition_id);
ALTER TABLE obligations ADD COLUMN IF NOT EXISTS definition_version INTEGER;
ALTER TABLE obligations DROP CONSTRAINT IF EXISTS chk_obligations_definition_pinned;
ALTER TABLE obligations ADD CONSTRAINT chk_obligations_definition_pinned
    CHECK ((obligation_definition_id IS NULL) = (definition_version IS NULL));

-- Versions and transition history are append-only: the evidence of what a
-- definition said when an obligation was raised from it, and of each filing
-- step, cannot be rewritten.
CREATE OR REPLACE FUNCTION obligations_refuse_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS trg_definition_versions_append_only ON obligation_definition_versions;
CREATE TRIGGER trg_definition_versions_append_only
    BEFORE UPDATE OR DELETE ON obligation_definition_versions
    FOR EACH ROW EXECUTE FUNCTION obligations_refuse_mutation();

DROP TRIGGER IF EXISTS trg_filing_transitions_append_only ON filing_requirement_transitions;
CREATE TRIGGER trg_filing_transitions_append_only
    BEFORE UPDATE OR DELETE ON filing_requirement_transitions
    FOR EACH ROW EXECUTE FUNCTION obligations_refuse_mutation();

-- Tenant isolation on every new table, NULLIF so an empty GUC is NULL, not a cast error.
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['filing_requirement_transitions', 'obligation_definitions',
                             'obligation_definition_versions', 'obligation_entity_mappings']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format('CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid)
            WITH CHECK (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid)', t);
    END LOOP;
END;
$$;

COMMIT;
