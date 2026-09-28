-- 000011_add_com04_usage_metering.up.sql
-- COM-04 Usage Metering (ZS-SVC-Q-001 §4.4; COM-CTRL-013..018; negative
-- paths #14-19, #37).
--
-- ── Scope boundary, stated once ────────────────────────────────────────────
--
-- §4.4 "Must not own": product price, subscription, arbitrary analytics
-- metrics. COM-04 does not know or care which price component references a
-- meter — that binding is read by COM-05 when it rates a certified
-- statement. COM-04 only needs the subscription to resolve WHICH TERM an
-- event's occurred_at falls in (the decision you made: one usage window per
-- subscription term, so COM-04 reuses COM-02's subscription_terms directly
-- rather than keeping its own parallel window calendar).
--
-- ── Certification assertions this schema exists to make provable ─────────
--
--   Completeness  watermark_at + event_count on the frozen statement.
--   Uniqueness    (meter_key, usage_event_id) is the ingestion primary key;
--                 a replay can never double-count.
--   Validity      only a registered meter is accepted; negative or
--                 malformed quantities are quarantined, never counted.
--   Cutoff        occurred_at (when it happened) and observed_at (when
--                 COM-04 saw it) are both kept.
--   Aggregation   meter_version is fixed on the statement; recomputing the
--                 same accepted rows under that version reproduces the same
--                 total (proved in Go, internal/domain/com04_usage.go).
--   Reconciliation event_count + quarantined_count on the statement let a
--                 reader check accepted+quarantined = everything ingested.
--
-- Custom SQLSTATE reused: CP001 immutable/append-only-only-change row.

-- ── Meter definitions (seller plane) ───────────────────────────────────────
--
-- A meter is registered once, at a version, and never edited — the same
-- immutable-version discipline as product_price_versions. unique_dimension
-- is required only for UNIQUE_COUNT (which dimension key values are counted
-- distinct); it means nothing for SUM/MAX/LAST and must be absent there.
CREATE TABLE meter_definitions (
    meter_key                VARCHAR(128) NOT NULL CHECK (meter_key ~ '^[a-z][a-z0-9_.:-]{0,127}$'),
    meter_version             INT          NOT NULL CHECK (meter_version >= 1),
    display_name              VARCHAR(255) NOT NULL CHECK (btrim(display_name) <> ''),
    unit                       VARCHAR(32)  NOT NULL,
    aggregation_method          VARCHAR(16)  NOT NULL CHECK (aggregation_method IN ('SUM', 'MAX', 'LAST', 'UNIQUE_COUNT')),
    unique_dimension             VARCHAR(64)  CHECK (unique_dimension ~ '^[a-z][a-z0-9_]{0,63}$'),
    allow_negative_correction     BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at                     TIMESTAMPTZ  NOT NULL,
    created_by_principal_id         VARCHAR(255) NOT NULL,
    retired_at                        TIMESTAMPTZ,
    retired_by_principal_id            VARCHAR(255),
    retire_reason                       TEXT,
    PRIMARY KEY (meter_key, meter_version),
    CONSTRAINT meter_definitions_unique_dimension_scope CHECK (
        (aggregation_method = 'UNIQUE_COUNT') = (unique_dimension IS NOT NULL)),
    CONSTRAINT meter_definitions_retire_evidence CHECK (
        (retired_at IS NULL) = (retired_by_principal_id IS NULL)
        AND (retired_at IS NULL) = (retire_reason IS NULL)
        AND (retire_reason IS NULL OR btrim(retire_reason) <> ''))
);

CREATE FUNCTION enforce_meter_definition_retire_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    retire_cols TEXT[] := ARRAY['retired_at', 'retired_by_principal_id', 'retire_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'meter definition %/% cannot be deleted; retire it', OLD.meter_key, OLD.meter_version
            USING ERRCODE = 'CP001';
    END IF;
    IF OLD.retired_at IS NOT NULL THEN
        RAISE EXCEPTION 'meter definition %/% is retired and final', OLD.meter_key, OLD.meter_version USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - retire_cols) IS DISTINCT FROM (to_jsonb(OLD) - retire_cols) THEN
        RAISE EXCEPTION 'meter definition %/% is never edited; register a new version and retire this one',
            OLD.meter_key, OLD.meter_version USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_meter_definitions_retire_only
    BEFORE UPDATE OR DELETE ON meter_definitions
    FOR EACH ROW EXECUTE FUNCTION enforce_meter_definition_retire_only();

ALTER TABLE meter_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE meter_definitions FORCE ROW LEVEL SECURITY;
CREATE POLICY meter_definitions_read ON meter_definitions FOR SELECT USING (true);
CREATE POLICY meter_definitions_seller_insert ON meter_definitions FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY meter_definitions_seller_update ON meter_definitions FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Usage statements (one per subscription/term/meter) ─────────────────────
--
-- Declared before usage_event_records because an event's optional
-- statement_id foreign key needs it. Created lazily, OPEN, the first time an
-- event lands in that window — never pre-created for windows nothing has
-- reported into.
CREATE TABLE usage_statements (
    statement_id              TEXT         PRIMARY KEY
        CHECK (statement_id ~ '^cust_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    subscription_id            TEXT         NOT NULL,
    term_no                     INT          NOT NULL,
    meter_key                    VARCHAR(128) NOT NULL,
    meter_version                 INT          NOT NULL,
    status                         VARCHAR(16)  NOT NULL CHECK (status IN ('OPEN', 'FROZEN', 'CERTIFIED', 'ADJUSTED', 'SUPERSEDED')),
    total_quantity                  NUMERIC      NOT NULL DEFAULT 0 CHECK (scale(total_quantity) <= 4),
    event_count                       INT          NOT NULL DEFAULT 0 CHECK (event_count >= 0),
    quarantined_count                   INT          NOT NULL DEFAULT 0 CHECK (quarantined_count >= 0),
    watermark_at                          TIMESTAMPTZ,
    created_at                              TIMESTAMPTZ  NOT NULL,
    frozen_at                                 TIMESTAMPTZ,
    frozen_by_principal_id                      VARCHAR(255),
    certified_at                                  TIMESTAMPTZ,
    certified_by_principal_id                       VARCHAR(255),
    superseded_at                                     TIMESTAMPTZ,
    superseded_by_principal_id                          VARCHAR(255),
    supersede_reason                                      TEXT,
    superseded_by_statement_id                              TEXT REFERENCES usage_statements (statement_id)
        DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (subscription_id, term_no) REFERENCES subscription_terms (subscription_id, term_no),
    FOREIGN KEY (meter_key, meter_version) REFERENCES meter_definitions (meter_key, meter_version),
    CONSTRAINT usage_statements_frozen_evidence CHECK (
        status = 'OPEN' OR (frozen_at IS NOT NULL AND frozen_by_principal_id IS NOT NULL)),
    CONSTRAINT usage_statements_certified_evidence CHECK (
        status NOT IN ('CERTIFIED', 'ADJUSTED') OR (certified_at IS NOT NULL AND certified_by_principal_id IS NOT NULL)),
    CONSTRAINT usage_statements_superseded_evidence CHECK (
        status <> 'SUPERSEDED' OR (superseded_at IS NOT NULL AND superseded_by_principal_id IS NOT NULL
            AND supersede_reason IS NOT NULL AND btrim(supersede_reason) <> '' AND superseded_by_statement_id IS NOT NULL))
);

CREATE INDEX idx_usage_statements_subscription ON usage_statements (subscription_id, meter_key);

-- Exactly one LIVE statement per window (COM-CTRL-018): ReopenWindow keeps
-- the superseded row rather than deleting it, so this excludes SUPERSEDED
-- instead of being a plain unique constraint. Postgres cannot attach a
-- partial index as a deferrable constraint, so ReopenWindow instead marks
-- the original SUPERSEDED first (satisfying this immediately — the row
-- drops out of the partial index) and inserts the OPEN replacement second;
-- the self-referencing superseded_by_statement_id foreign key that
-- ordering conflicts with is the one declared DEFERRABLE below.
CREATE UNIQUE INDEX idx_usage_statements_one_live_per_window
    ON usage_statements (subscription_id, term_no, meter_key) WHERE status <> 'SUPERSEDED';

-- Lifecycle trigger: each transition names the columns it may change; the
-- accumulator columns (total_quantity, event_count, quarantined_count,
-- watermark_at) may only move while OPEN — freezing is exactly the act of
-- locking them.
CREATE FUNCTION enforce_usage_statement_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    allowed TEXT[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'usage statement % cannot be deleted', OLD.statement_id USING ERRCODE = 'CP001';
    END IF;
    allowed := CASE
        WHEN OLD.status = 'OPEN' AND NEW.status = 'OPEN' THEN
            ARRAY['total_quantity', 'event_count', 'quarantined_count', 'watermark_at']
        WHEN OLD.status = 'OPEN' AND NEW.status = 'FROZEN' THEN
            ARRAY['status', 'frozen_at', 'frozen_by_principal_id']
        WHEN OLD.status = 'FROZEN' AND NEW.status IN ('CERTIFIED', 'ADJUSTED') THEN
            ARRAY['status', 'total_quantity', 'certified_at', 'certified_by_principal_id']
        WHEN OLD.status IN ('CERTIFIED', 'ADJUSTED') AND NEW.status = 'SUPERSEDED' THEN
            ARRAY['status', 'superseded_at', 'superseded_by_principal_id', 'supersede_reason', 'superseded_by_statement_id']
        ELSE NULL
    END;
    IF allowed IS NULL THEN
        RAISE EXCEPTION 'usage statement % cannot move from % to %', OLD.statement_id, OLD.status, NEW.status
            USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - allowed) IS DISTINCT FROM (to_jsonb(OLD) - allowed) THEN
        RAISE EXCEPTION 'usage statement %: protected fields cannot change on a % -> % transition',
            OLD.statement_id, OLD.status, NEW.status USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_usage_statements_lifecycle
    BEFORE UPDATE OR DELETE ON usage_statements
    FOR EACH ROW EXECUTE FUNCTION enforce_usage_statement_lifecycle();

-- ── Usage event records ────────────────────────────────────────────────────
--
-- (meter_key, usage_event_id) is the ingestion identity: a replay of the
-- same event under the same meter can never double-count, structurally
-- (COM-CTRL-014). late marks an event that arrived after the statement it
-- would have belonged to was already CERTIFIED/ADJUSTED — its quantity was
-- never added to that statement; an adjustment record carries it forward
-- instead (COM-CTRL-017).
CREATE TABLE usage_event_records (
    meter_key                VARCHAR(128) NOT NULL,
    usage_event_id             TEXT         NOT NULL CHECK (btrim(usage_event_id) <> ''),
    meter_version                INT          NOT NULL,
    organization_id                UUID         NOT NULL,
    subscription_id                  TEXT         NOT NULL,
    term_no                            INT          NOT NULL,
    statement_id                        TEXT         REFERENCES usage_statements (statement_id),
    quantity                              NUMERIC      NOT NULL CHECK (scale(quantity) <= 4),
    dimensions                              JSONB        NOT NULL DEFAULT '{}',
    occurred_at                               TIMESTAMPTZ  NOT NULL,
    observed_at                                 TIMESTAMPTZ  NOT NULL,
    source_service                                VARCHAR(128) NOT NULL,
    status                                          VARCHAR(16)  NOT NULL CHECK (status IN ('ACCEPTED', 'QUARANTINED', 'CORRECTED')),
    quarantine_reason                                 TEXT,
    late                                                BOOLEAN      NOT NULL DEFAULT FALSE,
    superseded_by_event_id                                TEXT,
    created_at                                              TIMESTAMPTZ  NOT NULL,
    PRIMARY KEY (meter_key, usage_event_id),
    FOREIGN KEY (meter_key, meter_version) REFERENCES meter_definitions (meter_key, meter_version),
    FOREIGN KEY (subscription_id, term_no) REFERENCES subscription_terms (subscription_id, term_no),
    FOREIGN KEY (meter_key, superseded_by_event_id) REFERENCES usage_event_records (meter_key, usage_event_id),
    CONSTRAINT usage_events_quantity_non_negative_unless_quarantined CHECK (status = 'QUARANTINED' OR quantity >= 0),
    CONSTRAINT usage_events_quarantine_evidence CHECK (
        (status = 'QUARANTINED') = (quarantine_reason IS NOT NULL) AND (quarantine_reason IS NULL OR btrim(quarantine_reason) <> '')),
    CONSTRAINT usage_events_corrected_evidence CHECK ((status = 'CORRECTED') = (superseded_by_event_id IS NOT NULL)),
    -- An accepted, on-time event always resolved to the statement it was
    -- counted into; a quarantined or late event never counted toward one.
    CONSTRAINT usage_events_statement_linkage CHECK (
        (status = 'ACCEPTED' AND NOT late AND statement_id IS NOT NULL)
        OR (status <> 'ACCEPTED') OR (status = 'ACCEPTED' AND late AND statement_id IS NULL))
);

CREATE INDEX idx_usage_events_statement ON usage_event_records (statement_id) WHERE statement_id IS NOT NULL;
CREATE INDEX idx_usage_events_subscription ON usage_event_records (subscription_id, meter_key, occurred_at);

-- Append-only: the one permitted change is marking an OPEN-window event
-- CORRECTED, pointing at its replacement.
-- Two permitted changes, never combined in one UPDATE: marking an event
-- CORRECTED (status + superseded_by_event_id), or re-homing an ACCEPTED
-- event's statement_id — the one thing ReopenWindow does, moving every
-- accepted event from a superseded statement onto its OPEN replacement so
-- it aggregates again on recertify. Everything else about the row is fixed
-- the instant it is written.
CREATE FUNCTION enforce_usage_event_correct_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    correct_cols TEXT[] := ARRAY['status', 'superseded_by_event_id'];
    rehome_cols  TEXT[] := ARRAY['statement_id'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'usage event %/% cannot be deleted', OLD.meter_key, OLD.usage_event_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.status = 'CORRECTED' THEN
        RAISE EXCEPTION 'usage event %/% is already corrected', OLD.meter_key, OLD.usage_event_id USING ERRCODE = 'CP001';
    END IF;
    IF NEW.status = 'CORRECTED' AND (to_jsonb(NEW) - correct_cols) IS NOT DISTINCT FROM (to_jsonb(OLD) - correct_cols) THEN
        RETURN NEW;
    END IF;
    IF NEW.status = OLD.status AND OLD.status = 'ACCEPTED'
       AND (to_jsonb(NEW) - rehome_cols) IS NOT DISTINCT FROM (to_jsonb(OLD) - rehome_cols) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'usage event %/% is append-only; the only changes are marking it CORRECTED or re-homing an accepted event''s statement_id',
        OLD.meter_key, OLD.usage_event_id USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_usage_event_records_correct_only
    BEFORE UPDATE OR DELETE ON usage_event_records
    FOR EACH ROW EXECUTE FUNCTION enforce_usage_event_correct_only();

-- ── Usage adjustments ───────────────────────────────────────────────────────
--
-- Carries a late event's quantity into the next OPEN statement instead of
-- rewriting the CERTIFIED/ADJUSTED one it would otherwise have belonged to.
-- Never updated: a wrong adjustment is corrected by recording another one,
-- same append-only doctrine as everything else in this schema.
CREATE TABLE usage_adjustments (
    adjustment_id             TEXT         PRIMARY KEY
        CHECK (adjustment_id ~ '^cuadj_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    origin_statement_id        TEXT         NOT NULL REFERENCES usage_statements (statement_id),
    target_statement_id         TEXT         NOT NULL REFERENCES usage_statements (statement_id),
    meter_key                    VARCHAR(128) NOT NULL,
    source_usage_event_id          TEXT         NOT NULL,
    quantity                         NUMERIC      NOT NULL CHECK (scale(quantity) <= 4),
    reason                              TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    created_at                           TIMESTAMPTZ  NOT NULL,
    created_by_principal_id                VARCHAR(255) NOT NULL,
    FOREIGN KEY (meter_key, source_usage_event_id) REFERENCES usage_event_records (meter_key, usage_event_id),
    CONSTRAINT usage_adjustments_distinct_statements CHECK (origin_statement_id <> target_statement_id)
);

CREATE INDEX idx_usage_adjustments_target ON usage_adjustments (target_statement_id);
CREATE INDEX idx_usage_adjustments_origin ON usage_adjustments (origin_statement_id);

CREATE TRIGGER trg_usage_adjustments_immutable
    BEFORE UPDATE OR DELETE ON usage_adjustments
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- ── Row-level security (tenant plane) ──────────────────────────────────────
--
-- Reads: the organization's own rows, or the seller/boundary_worker plane.
-- Writes: seller/boundary_worker plane only — ingestion is a granted
-- workload acting on a named organization, not that organization acting on
-- itself (COM-CTRL-015; negative path #17), and freeze/certify/supersede are
-- likewise platform/worker actions, never the tenant's own.
ALTER TABLE usage_statements ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_statements FORCE ROW LEVEL SECURITY;
CREATE POLICY usage_statements_access ON usage_statements FOR ALL
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker')
           OR subscription_id IN (SELECT subscription_id FROM subscriptions))
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));

ALTER TABLE usage_event_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_event_records FORCE ROW LEVEL SECURITY;
CREATE POLICY usage_events_access ON usage_event_records FOR ALL
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker')
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));

ALTER TABLE usage_adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE usage_adjustments FORCE ROW LEVEL SECURITY;
CREATE POLICY usage_adjustments_access ON usage_adjustments FOR ALL
    USING (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker')
           OR origin_statement_id IN (SELECT statement_id FROM usage_statements))
    WITH CHECK (current_setting('app.commercial_plane', true) IN ('seller', 'boundary_worker'));
