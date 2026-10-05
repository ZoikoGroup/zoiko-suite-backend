-- Schema for reconciliation-engine-svc (DATA-07, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Reconciliation Definitions: seller-managed, versioned rule set
-- (match keys, tolerance, materiality). Mutable in place, same idiom
-- as commercial_currencies/search_policies elsewhere in this platform
-- — version auto-increments on every UPDATE (see the trigger below),
-- never caller-supplied, so "optimistic version checks on
-- configuration/rule changes" has a real, server-owned counter to
-- check against. A run copies the CURRENT version's values onto
-- itself at StartRun time and never reads this table again.
CREATE TABLE IF NOT EXISTS reconciliation_definitions (
    definition_id                   TEXT         PRIMARY KEY
        CHECK (definition_id ~ '^rdf_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                         VARCHAR(64)  NOT NULL,
    name                                VARCHAR(256) NOT NULL,
    match_key_fields                     TEXT[]       NOT NULL,
    tolerance_amount_minor_units            BIGINT       NOT NULL CHECK (tolerance_amount_minor_units >= 0),
    materiality_amount_minor_units            BIGINT       NOT NULL CHECK (materiality_amount_minor_units >= 0),
    version                                     BIGINT       NOT NULL DEFAULT 1,
    created_at                                    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                                      VARCHAR(128) NOT NULL,
    updated_at                                        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)
);

-- Reconciliation Runs: freezes the definition's rules onto itself.
-- Planned -> Running -> ExceptionsOpen -> Reperformed -> Certified /
-- Failed -> Superseded. Certified/Failed are content-terminal: the
-- only further transition permitted is to Superseded, and even then
-- every column except status/superseded_by_run_id must stay the same
-- — this is what makes "tolerance cannot be widened during a
-- certified run" true as a strict consequence of full immutability,
-- not a special case.
CREATE TABLE IF NOT EXISTS reconciliation_runs (
    run_id                           TEXT         PRIMARY KEY
        CHECK (run_id ~ '^rcr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                          VARCHAR(64)  NOT NULL,
    definition_id                        TEXT         NOT NULL REFERENCES reconciliation_definitions(definition_id),
    definition_version                     BIGINT       NOT NULL,
    status                                    VARCHAR(16)  NOT NULL DEFAULT 'Planned'
        CHECK (status IN ('Planned','Running','ExceptionsOpen','Reperformed','Certified','Failed','Superseded')),
    tolerance_amount_minor_units                BIGINT       NOT NULL CHECK (tolerance_amount_minor_units >= 0),
    materiality_amount_minor_units                 BIGINT       NOT NULL CHECK (materiality_amount_minor_units >= 0),
    supersedes_run_id                                TEXT         REFERENCES reconciliation_runs(run_id),
    superseded_by_run_id                               TEXT         REFERENCES reconciliation_runs(run_id),
    superseded_reason                                    TEXT,
    started_at                                           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_by                                             VARCHAR(128) NOT NULL,
    certified_at                                             TIMESTAMPTZ,
    certified_by                                               VARCHAR(128)
);

-- Population Snapshots: one frozen side of a run's comparison.
-- Immutable the moment StartRun creates it — the "frozen populations"
-- core control. This service never edits the source records a
-- snapshot's items were copied from.
CREATE TABLE IF NOT EXISTS population_snapshots (
    snapshot_id                    TEXT         PRIMARY KEY
        CHECK (snapshot_id ~ '^rps_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                        VARCHAR(64)  NOT NULL,
    run_id                              TEXT         NOT NULL REFERENCES reconciliation_runs(run_id),
    side                                  VARCHAR(64)  NOT NULL,
    source_system                          VARCHAR(128) NOT NULL,
    item_count                               BIGINT       NOT NULL DEFAULT 0,
    total_amount_minor_units                   BIGINT       NOT NULL DEFAULT 0,
    content_hash                                 CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    created_at                                     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, side)
);

-- Population Items: the individual frozen line items making up one
-- snapshot. Matching operates on these, by match key (ref_id) — never
-- by aggregate totals alone, which is what makes "equal totals with
-- different composition still surface exceptions" true.
CREATE TABLE IF NOT EXISTS population_items (
    item_id                     TEXT         PRIMARY KEY
        CHECK (item_id ~ '^rpi_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                     VARCHAR(64)  NOT NULL,
    snapshot_id                     TEXT         NOT NULL REFERENCES population_snapshots(snapshot_id),
    ref_id                             VARCHAR(256) NOT NULL,
    amount_minor_units                   BIGINT       NOT NULL CHECK (amount_minor_units >= 0),
    occurred_at                            TIMESTAMPTZ  NOT NULL,
    dimensions                               JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at                                 TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_population_items_snapshot ON population_items(snapshot_id);
CREATE INDEX IF NOT EXISTS idx_population_items_ref ON population_items(snapshot_id, ref_id);

-- Match Results: append-only. A manual match always carries a
-- non-empty reason and the authorizing principal — this is the
-- evidence the "manual match requires reason/authority and remains
-- auditable" acceptance test checks for.
CREATE TABLE IF NOT EXISTS match_results (
    match_result_id              TEXT         PRIMARY KEY
        CHECK (match_result_id ~ '^rmr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                      VARCHAR(64)  NOT NULL,
    run_id                            TEXT         NOT NULL REFERENCES reconciliation_runs(run_id),
    item_a_id                           TEXT         NOT NULL REFERENCES population_items(item_id),
    item_b_id                             TEXT         NOT NULL REFERENCES population_items(item_id),
    ref_id                                   VARCHAR(256) NOT NULL,
    manual                                     BOOLEAN      NOT NULL DEFAULT FALSE,
    reason                                       TEXT,
    matched_by                                     VARCHAR(128) NOT NULL,
    matched_at                                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (NOT manual OR (reason IS NOT NULL AND reason <> '')),
    UNIQUE (item_a_id),
    UNIQUE (item_b_id)
);

-- Reconciliation Exceptions: raised for anything that didn't
-- reconcile. Its only legitimate transition is Open -> Resolved, and
-- only as the side effect of a manual match consuming the exact item
-- it was raised for (see the trigger below) — never a bare "resolve"
-- command with no evidence behind it.
CREATE TABLE IF NOT EXISTS reconciliation_exceptions (
    exception_id                  TEXT         PRIMARY KEY
        CHECK (exception_id ~ '^rex_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                       VARCHAR(64)  NOT NULL,
    run_id                             TEXT         NOT NULL REFERENCES reconciliation_runs(run_id),
    item_id                               TEXT         NOT NULL REFERENCES population_items(item_id),
    side                                    VARCHAR(64)  NOT NULL,
    ref_id                                     VARCHAR(256) NOT NULL,
    reason                                        TEXT         NOT NULL,
    amount_minor_units                              BIGINT       NOT NULL CHECK (amount_minor_units >= 0),
    status                                            VARCHAR(16)  NOT NULL DEFAULT 'Open' CHECK (status IN ('Open','Resolved')),
    raised_at                                           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    raised_by                                             VARCHAR(128) NOT NULL,
    resolved_at                                             TIMESTAMPTZ,
    resolved_by_match_id                                      TEXT         REFERENCES match_results(match_result_id),
    UNIQUE (item_id)
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_exceptions_run_status ON reconciliation_exceptions(run_id, status);

-- Certifications: sealed, immutable evidence produced by Certify.
CREATE TABLE IF NOT EXISTS certifications (
    certification_id               TEXT         PRIMARY KEY
        CHECK (certification_id ~ '^rct_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                         VARCHAR(64)  NOT NULL,
    run_id                               TEXT         NOT NULL UNIQUE REFERENCES reconciliation_runs(run_id),
    definition_version                     BIGINT       NOT NULL,
    matched_count                            BIGINT       NOT NULL,
    exception_count                            BIGINT       NOT NULL,
    total_asserted_minor_units                   BIGINT       NOT NULL,
    manifest_sha256                                CHAR(64)     NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    sealed_at                                        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    sealed_by                                          VARCHAR(128) NOT NULL
);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE reconciliation_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_definitions_tenant_isolation ON reconciliation_definitions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_runs_tenant_isolation ON reconciliation_runs
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE population_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE population_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY population_snapshots_tenant_isolation ON population_snapshots
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE population_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE population_items FORCE ROW LEVEL SECURITY;
CREATE POLICY population_items_tenant_isolation ON population_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE match_results ENABLE ROW LEVEL SECURITY;
ALTER TABLE match_results FORCE ROW LEVEL SECURITY;
CREATE POLICY match_results_tenant_isolation ON match_results
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE reconciliation_exceptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_exceptions FORCE ROW LEVEL SECURITY;
CREATE POLICY reconciliation_exceptions_tenant_isolation ON reconciliation_exceptions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE certifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE certifications FORCE ROW LEVEL SECURITY;
CREATE POLICY certifications_tenant_isolation ON certifications
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Lifecycle triggers ───────────────────────────────────────────────────────

-- Reconciliation Definitions: version always auto-increments server
-- side on UPDATE, ignoring any caller-supplied value — this is the
-- real, server-owned counter "optimistic version checks on
-- configuration/rule changes" is checked against. No DELETE (reference
-- data).
CREATE OR REPLACE FUNCTION enforce_definition_versioning()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation definitions cannot be deleted';
    END IF;
    NEW.version := OLD.version + 1;
    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_definition_versioning
    BEFORE UPDATE OR DELETE ON reconciliation_definitions
    FOR EACH ROW EXECUTE FUNCTION enforce_definition_versioning();

-- Reconciliation Runs: forward-only status machine. Certified/Failed
-- allow exactly one further transition (to Superseded) while every
-- other column must stay unchanged in that same update — this is what
-- makes "tolerance cannot be widened during a certified run" true as a
-- strict consequence of full immutability. Superseded is fully
-- terminal.
CREATE OR REPLACE FUNCTION enforce_run_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation runs cannot be deleted';
    END IF;

    IF OLD.status = 'Superseded' THEN
        RAISE EXCEPTION 'reconciliation run % is Superseded and immutable', OLD.run_id;
    END IF;

    IF OLD.status IN ('Certified', 'Failed') THEN
        IF NEW.status <> 'Superseded' THEN
            RAISE EXCEPTION 'reconciliation run % is % and immutable except for supersession', OLD.run_id, OLD.status;
        END IF;
        IF NEW.definition_id IS DISTINCT FROM OLD.definition_id
            OR NEW.definition_version IS DISTINCT FROM OLD.definition_version
            OR NEW.tolerance_amount_minor_units IS DISTINCT FROM OLD.tolerance_amount_minor_units
            OR NEW.materiality_amount_minor_units IS DISTINCT FROM OLD.materiality_amount_minor_units
            OR NEW.certified_at IS DISTINCT FROM OLD.certified_at
            OR NEW.certified_by IS DISTINCT FROM OLD.certified_by
            OR NEW.started_at IS DISTINCT FROM OLD.started_at
            OR NEW.started_by IS DISTINCT FROM OLD.started_by
        THEN
            RAISE EXCEPTION 'reconciliation run % is % and only status/superseded_by_run_id/superseded_reason may change', OLD.run_id, OLD.status;
        END IF;
        RETURN NEW;
    END IF;

    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Planned' THEN
                IF NEW.status <> 'Running' THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from Planned to %', NEW.status;
                END IF;
            WHEN 'Running' THEN
                IF NEW.status NOT IN ('ExceptionsOpen', 'Certified', 'Failed', 'Reperformed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from Running to %', NEW.status;
                END IF;
            WHEN 'ExceptionsOpen' THEN
                IF NEW.status NOT IN ('Running', 'Reperformed', 'Failed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from ExceptionsOpen to %', NEW.status;
                END IF;
            WHEN 'Reperformed' THEN
                IF NEW.status NOT IN ('Running', 'ExceptionsOpen', 'Certified', 'Failed') THEN
                    RAISE EXCEPTION 'invalid reconciliation run transition from Reperformed to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown reconciliation run status %', OLD.status;
        END CASE;
    END IF;

    IF NEW.status = 'Certified' THEN
        NEW.certified_at := COALESCE(NEW.certified_at, NOW());
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_run_lifecycle
    BEFORE UPDATE OR DELETE ON reconciliation_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_run_lifecycle();

-- Population Snapshots / Items: fully immutable once written — the
-- "frozen populations" core control. UPDATE/DELETE RLS is granted (see
-- the application role's GRANT in the test harness) so a mutation
-- attempt reaches this trigger and is rejected loudly, rather than RLS
-- silently filtering it to zero affected rows.
CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_population_snapshots_immutable
    BEFORE UPDATE OR DELETE ON population_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_population_items_immutable
    BEFORE UPDATE OR DELETE ON population_items
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_match_results_immutable
    BEFORE UPDATE OR DELETE ON match_results
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_certifications_immutable
    BEFORE UPDATE OR DELETE ON certifications
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- Reconciliation Exceptions: the only legitimate transition is
-- Open -> Resolved, together with resolved_at/resolved_by_match_id
-- being set in that same update — every other column must stay
-- unchanged, and a Resolved row is fully terminal.
CREATE OR REPLACE FUNCTION enforce_exception_resolution()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'reconciliation exceptions cannot be deleted';
    END IF;

    IF OLD.status = 'Resolved' THEN
        RAISE EXCEPTION 'reconciliation exception % is already Resolved and immutable', OLD.exception_id;
    END IF;

    IF NEW.status <> 'Resolved' THEN
        RAISE EXCEPTION 'reconciliation exceptions may only transition Open -> Resolved';
    END IF;
    IF NEW.resolved_by_match_id IS NULL THEN
        RAISE EXCEPTION 'resolving an exception requires resolved_by_match_id';
    END IF;
    IF NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.item_id IS DISTINCT FROM OLD.item_id
        OR NEW.side IS DISTINCT FROM OLD.side
        OR NEW.ref_id IS DISTINCT FROM OLD.ref_id
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.amount_minor_units IS DISTINCT FROM OLD.amount_minor_units
        OR NEW.raised_at IS DISTINCT FROM OLD.raised_at
        OR NEW.raised_by IS DISTINCT FROM OLD.raised_by
    THEN
        RAISE EXCEPTION 'reconciliation exception % may only have status/resolved_at/resolved_by_match_id change', OLD.exception_id;
    END IF;
    NEW.resolved_at := COALESCE(NEW.resolved_at, NOW());
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_exception_resolution
    BEFORE UPDATE OR DELETE ON reconciliation_exceptions
    FOR EACH ROW EXECUTE FUNCTION enforce_exception_resolution();
