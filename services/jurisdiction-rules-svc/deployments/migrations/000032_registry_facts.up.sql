-- 000032_registry_facts.up.sql
-- JUR-1: the establishment (ORG-6) and registration (ORG-7) facts a jurisdiction decision rests on, projected from
-- tenant-entity-registry-svc's establishment.*, registration.* and address.* events. The registry stays the system
-- of record. One row per status event, so the state on any past date can be rebuilt and a later deactivation or
-- expiry is never read back into an earlier decision.
--
-- Tenant data in a platform service: every read filters on tenant_id. Ids are TEXT so a malformed id from a caller
-- is a non-match, never a cast error.

CREATE TABLE IF NOT EXISTS registry_fact_events (
    event_id        TEXT         PRIMARY KEY,
    tenant_id       TEXT         NOT NULL,
    legal_entity_id TEXT         NOT NULL,
    fact_kind       VARCHAR(16)  NOT NULL,
    fact_id         TEXT         NOT NULL,
    subtype         VARCHAR(64)  NOT NULL DEFAULT '',
    -- registration: its jurisdiction code
    jurisdiction    VARCHAR(16)  NOT NULL DEFAULT '',
    -- establishment: its address, whose country is in registry_address_countries
    address_id      TEXT         NOT NULL DEFAULT '',
    status          VARCHAR(16)  NOT NULL,
    effective_date  DATE         NOT NULL,
    expiry_date     DATE,
    object_version  BIGINT       NOT NULL,
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_registry_fact_kind CHECK (fact_kind IN ('ESTABLISHMENT', 'REGISTRATION')),
    CONSTRAINT uq_registry_fact_version UNIQUE (tenant_id, fact_kind, fact_id, object_version)
);

CREATE INDEX IF NOT EXISTS idx_registry_fact_entity
    ON registry_fact_events (tenant_id, legal_entity_id, effective_date);

-- The country of an address, at its latest version seen. A superseded version carries no country, so it is
-- stored as unknown (empty) until a later version says otherwise: a decision then names the fact it cannot use.
CREATE TABLE IF NOT EXISTS registry_address_countries (
    tenant_id      TEXT        NOT NULL,
    address_id     TEXT        NOT NULL,
    country        VARCHAR(16) NOT NULL DEFAULT '',
    object_version BIGINT      NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, address_id)
);

DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT ON registry_fact_events TO app_jurisdiction_rules;
        GRANT SELECT, INSERT, UPDATE ON registry_address_countries TO app_jurisdiction_rules;
    END IF;
END $$;
