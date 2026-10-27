-- 000019_entity_jurisdiction_applicability.up.sql
-- Doc 03 §8.2: this service owns "entity-jurisdiction applicability mappings"
-- and consumes entity.created and entity.jurisdiction.changed. Neither was
-- consumed. tenant-entity-registry-svc remains the system of record for
-- assignments; this table is this service's projection of them, so it can
-- answer "which jurisdictions apply to this entity at date X" itself.
--
-- Tenant-scoped data in a platform service: every read filters on tenant_id.
-- tenant_id and legal_entity_id are TEXT so a malformed id from a caller is a
-- non-match, never a uuid cast error answered as 503.

CREATE TABLE IF NOT EXISTS entity_jurisdiction_applicability (
    -- assignment_id for entity.jurisdiction.changed; 'primary:<legal_entity_id>'
    -- for the primary jurisdiction named by entity.created.
    applicability_id      TEXT         PRIMARY KEY,
    tenant_id             TEXT         NOT NULL,
    legal_entity_id       TEXT         NOT NULL,
    jurisdiction_id       UUID         NOT NULL,
    -- ENTITY_PRIMARY (entity.created) or the producer's assignment_type
    -- (PRIMARY, SECONDARY, TAX_ONLY, FILING_ONLY).
    applicability_type    VARCHAR(32)  NOT NULL,
    effective_from        TIMESTAMPTZ  NOT NULL,
    effective_to          TIMESTAMPTZ,
    -- The producer's object_version: a stale redelivery never overwrites a newer state.
    source_object_version BIGINT       NOT NULL,
    source_event_type     VARCHAR(64)  NOT NULL,
    source_event_id       TEXT         NOT NULL,
    evidence_ref          TEXT,
    recorded_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_applicability_period CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE INDEX IF NOT EXISTS idx_applicability_entity
    ON entity_jurisdiction_applicability (tenant_id, legal_entity_id, effective_from);

-- Durable de-duplication for every consumed event: the producer's relay
-- redelivers on a partial batch failure, and an in-process set forgets on restart.
CREATE TABLE IF NOT EXISTS consumed_events (
    event_id     TEXT         PRIMARY KEY,
    event_type   VARCHAR(64)  NOT NULL,
    consumed_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON entity_jurisdiction_applicability TO app_jurisdiction_rules;
        GRANT SELECT, INSERT ON consumed_events TO app_jurisdiction_rules;
    END IF;
END $$;
