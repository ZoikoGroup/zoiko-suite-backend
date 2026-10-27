-- 011 ZS-SVC-T-001 Wave 8: staged production rollout and migration of ad-hoc connectors (§18.2, §18.3).
--
-- A production connection goes live in stages: PLANNED -> PILOT (an operation allowlist and a daily budget) -> GENERAL.
-- A step forward is a change request with readiness evidence, decided by someone other than the requester; a step
-- back (rollback) is immediate. An ad-hoc connector that already exists in a domain service is brought in through an
-- inventory entry: DISCOVERED -> MAPPED -> SHADOW -> CUTOVER -> RETIRED. In SHADOW the owning service registers its
-- attempts into the generic exchange history and a parity run compares them with what the platform holds; CUTOVER
-- needs a passed parity run and a second person.
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd) on every table;
--   * a rollout or legacy connector keeps its identity; stages and states move only along the allowed edges, and a
--     move forward exists only with an APPROVED change request of that subject, for exactly that edge, decided by
--     someone other than its requester (enforced here, not only in the service);
--   * a legacy exchange registration and a parity run are append-only; a registration never changes after it is made;
--   * the daily pilot budget is one counter per rollout and day, taken atomically.

CREATE TABLE IF NOT EXISTS xic_rollout_requests (
    request_id    UUID         PRIMARY KEY,
    tenant_id     VARCHAR(64)  NOT NULL,
    subject_kind  VARCHAR(8)   NOT NULL,
    subject_id    UUID         NOT NULL,
    from_state    VARCHAR(12)  NOT NULL,
    to_state      VARCHAR(12)  NOT NULL,
    status        VARCHAR(8)   NOT NULL DEFAULT 'PENDING',
    attestations  JSONB        NOT NULL DEFAULT '[]',
    readiness     JSONB        NOT NULL DEFAULT '{}',
    reason        VARCHAR(1000) NOT NULL DEFAULT '',
    requested_by  VARCHAR(255) NOT NULL,
    decided_by    VARCHAR(255) NOT NULL DEFAULT '',
    decision_note VARCHAR(1000) NOT NULL DEFAULT '',
    expires_at    TIMESTAMPTZ  NOT NULL,
    decided_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    version       BIGINT       NOT NULL DEFAULT 1,
    CONSTRAINT xic_rq_kind CHECK (subject_kind IN ('ROLLOUT', 'LEGACY')),
    CONSTRAINT xic_rq_status CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'EXPIRED', 'USED')),
    CONSTRAINT xic_rq_decider CHECK (status = 'PENDING' OR status = 'EXPIRED' OR (decided_by <> '' AND decided_by <> requested_by))
);
CREATE INDEX IF NOT EXISTS xic_rq_subject ON xic_rollout_requests (subject_kind, subject_id, created_at);
-- one open request per subject and edge
CREATE UNIQUE INDEX IF NOT EXISTS xic_rq_one_open ON xic_rollout_requests (subject_kind, subject_id, to_state) WHERE status = 'PENDING';

CREATE OR REPLACE FUNCTION xic_rq_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a rollout request is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.request_id <> OLD.request_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.subject_kind <> OLD.subject_kind OR NEW.subject_id <> OLD.subject_id
       OR NEW.from_state <> OLD.from_state OR NEW.to_state <> OLD.to_state OR NEW.requested_by <> OLD.requested_by
       OR NEW.attestations::text <> OLD.attestations::text OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a rollout request is fixed once made' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('REJECTED', 'EXPIRED', 'USED') THEN
        RAISE EXCEPTION 'a % request is final', OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (OLD.status = NEW.status
         OR (OLD.status = 'PENDING' AND NEW.status IN ('APPROVED', 'REJECTED', 'EXPIRED'))
         OR (OLD.status = 'APPROVED' AND NEW.status IN ('USED', 'EXPIRED'))) THEN
        RAISE EXCEPTION 'request status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_rq_guard_trg ON xic_rollout_requests;
CREATE TRIGGER xic_rq_guard_trg BEFORE UPDATE OR DELETE ON xic_rollout_requests FOR EACH ROW EXECUTE FUNCTION xic_rq_guard();

CREATE TABLE IF NOT EXISTS xic_rollouts (
    rollout_id        UUID         PRIMARY KEY,
    tenant_id         VARCHAR(64)  NOT NULL,
    connection_id     UUID         NOT NULL REFERENCES xic_connections(connection_id),
    connector_id      VARCHAR(63)  NOT NULL,
    stage             VARCHAR(11)  NOT NULL DEFAULT 'PLANNED',
    pilot_ops         TEXT[]       NOT NULL DEFAULT '{}',
    pilot_daily_cap   INTEGER      NOT NULL,
    min_soak_hours    INTEGER      NOT NULL,
    min_success       INTEGER      NOT NULL,
    max_failure_pct   INTEGER      NOT NULL,
    stage_since       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    advance_request   UUID         REFERENCES xic_rollout_requests(request_id),
    rollback_reason   VARCHAR(1000) NOT NULL DEFAULT '',
    rolled_back_by    VARCHAR(255) NOT NULL DEFAULT '',
    created_by        VARCHAR(255) NOT NULL,
    version           BIGINT       NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_ro_stage CHECK (stage IN ('PLANNED', 'PILOT', 'GENERAL', 'ROLLED_BACK')),
    CONSTRAINT xic_ro_limits CHECK (pilot_daily_cap >= 1 AND min_soak_hours >= 0 AND min_success >= 0 AND max_failure_pct BETWEEN 0 AND 100 AND version >= 1),
    CONSTRAINT xic_ro_rolled_back CHECK (stage <> 'ROLLED_BACK' OR (rollback_reason <> '' AND rolled_back_by <> ''))
);
-- a connection has one live rollout; a rolled-back one is history
CREATE UNIQUE INDEX IF NOT EXISTS xic_ro_live ON xic_rollouts (connection_id) WHERE stage <> 'ROLLED_BACK';
CREATE INDEX IF NOT EXISTS xic_ro_pilot ON xic_rollouts (stage) WHERE stage = 'PILOT';

CREATE OR REPLACE FUNCTION xic_ro_guard() RETURNS trigger AS $$
DECLARE r xic_rollout_requests%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a rollout is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.rollout_id <> OLD.rollout_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.connection_id <> OLD.connection_id
       OR NEW.connector_id <> OLD.connector_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity of a rollout is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'rollout version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.stage = 'ROLLED_BACK' THEN
        RAISE EXCEPTION 'a rolled-back rollout is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.stage <> NEW.stage THEN
        IF NEW.stage = 'ROLLED_BACK' THEN
            RETURN NEW;  -- the way back is always open
        END IF;
        IF NOT ((OLD.stage = 'PLANNED' AND NEW.stage = 'PILOT') OR (OLD.stage = 'PILOT' AND NEW.stage = 'GENERAL')) THEN
            RAISE EXCEPTION 'rollout stage cannot go from % to %', OLD.stage, NEW.stage USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        SELECT * INTO r FROM xic_rollout_requests WHERE request_id = NEW.advance_request;
        IF NOT FOUND OR r.subject_kind <> 'ROLLOUT' OR r.subject_id <> NEW.rollout_id OR r.from_state <> OLD.stage OR r.to_state <> NEW.stage
           OR r.status <> 'USED' OR r.decided_by = '' OR r.decided_by = r.requested_by THEN
            RAISE EXCEPTION 'a rollout moves forward only through an approved request decided by someone else' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF OLD.pilot_ops <> NEW.pilot_ops OR OLD.pilot_daily_cap <> NEW.pilot_daily_cap OR OLD.min_soak_hours <> NEW.min_soak_hours
          OR OLD.min_success <> NEW.min_success OR OLD.max_failure_pct <> NEW.max_failure_pct THEN
        IF OLD.stage <> 'PLANNED' THEN
            RAISE EXCEPTION 'the pilot terms are fixed once the pilot starts' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_ro_guard_trg ON xic_rollouts;
CREATE TRIGGER xic_ro_guard_trg BEFORE UPDATE OR DELETE ON xic_rollouts FOR EACH ROW EXECUTE FUNCTION xic_ro_guard();

CREATE TABLE IF NOT EXISTS xic_rollout_usage (
    rollout_id UUID        NOT NULL REFERENCES xic_rollouts(rollout_id),
    tenant_id  VARCHAR(64) NOT NULL,
    day        DATE        NOT NULL,
    admitted   INTEGER     NOT NULL DEFAULT 0,
    PRIMARY KEY (rollout_id, day),
    CONSTRAINT xic_ru_nonneg CHECK (admitted >= 0)
);

CREATE TABLE IF NOT EXISTS xic_rollout_events (
    event_id     UUID         PRIMARY KEY,
    tenant_id    VARCHAR(64)  NOT NULL,
    subject_kind VARCHAR(8)   NOT NULL,
    subject_id   UUID         NOT NULL,
    kind         VARCHAR(32)  NOT NULL,
    actor_id     VARCHAR(255) NOT NULL DEFAULT '',
    detail       JSONB,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS xic_rev_subject ON xic_rollout_events (subject_kind, subject_id, created_at);
DROP TRIGGER IF EXISTS xic_rollout_events_no_change ON xic_rollout_events;
CREATE TRIGGER xic_rollout_events_no_change BEFORE UPDATE OR DELETE ON xic_rollout_events FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- ── ad-hoc connector migration ──────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS xic_legacy_connectors (
    legacy_id       UUID         PRIMARY KEY,
    tenant_id       VARCHAR(64)  NOT NULL,
    name            VARCHAR(80)  NOT NULL,
    owning_service  VARCHAR(80)  NOT NULL,
    direction       VARCHAR(8)   NOT NULL,
    description     VARCHAR(1000) NOT NULL DEFAULT '',
    state           VARCHAR(10)  NOT NULL DEFAULT 'DISCOVERED',
    connection_id   UUID         REFERENCES xic_connections(connection_id),
    state_since     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    advance_request UUID         REFERENCES xic_rollout_requests(request_id),
    created_by      VARCHAR(255) NOT NULL,
    version         BIGINT       NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_lc_name UNIQUE (tenant_id, name),
    CONSTRAINT xic_lc_direction CHECK (direction IN ('INBOUND', 'OUTBOUND', 'BATCH')),
    CONSTRAINT xic_lc_state CHECK (state IN ('DISCOVERED', 'MAPPED', 'SHADOW', 'CUTOVER', 'RETIRED')),
    CONSTRAINT xic_lc_mapped CHECK (state = 'DISCOVERED' OR connection_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS xic_lc_conn ON xic_legacy_connectors (connection_id);

CREATE OR REPLACE FUNCTION xic_lc_guard() RETURNS trigger AS $$
DECLARE r xic_rollout_requests%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a legacy connector is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.legacy_id <> OLD.legacy_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.name <> OLD.name OR NEW.owning_service <> OLD.owning_service
       OR NEW.direction <> OLD.direction OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity of a legacy connector is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'legacy connector version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'RETIRED' THEN
        RAISE EXCEPTION 'a retired legacy connector is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.connection_id IS NOT NULL AND NEW.connection_id IS DISTINCT FROM OLD.connection_id AND OLD.state IN ('SHADOW', 'CUTOVER') THEN
        RAISE EXCEPTION 'the target connection cannot change while shadowing or after cutover' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> NEW.state THEN
        IF NOT ((OLD.state = 'DISCOVERED' AND NEW.state = 'MAPPED') OR (OLD.state = 'MAPPED' AND NEW.state = 'SHADOW')
             OR (OLD.state = 'SHADOW' AND NEW.state IN ('MAPPED', 'CUTOVER'))
             OR (OLD.state = 'CUTOVER' AND NEW.state IN ('SHADOW', 'RETIRED'))) THEN
            RAISE EXCEPTION 'legacy state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        -- cutover and retirement are decided by a second person; going back is always open
        IF NEW.state IN ('CUTOVER', 'RETIRED') THEN
            SELECT * INTO r FROM xic_rollout_requests WHERE request_id = NEW.advance_request;
            IF NOT FOUND OR r.subject_kind <> 'LEGACY' OR r.subject_id <> NEW.legacy_id OR r.from_state <> OLD.state OR r.to_state <> NEW.state
               OR r.status <> 'USED' OR r.decided_by = '' OR r.decided_by = r.requested_by THEN
                RAISE EXCEPTION 'cutover and retirement need an approved request decided by someone else' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_lc_guard_trg ON xic_legacy_connectors;
CREATE TRIGGER xic_lc_guard_trg BEFORE UPDATE OR DELETE ON xic_legacy_connectors FOR EACH ROW EXECUTE FUNCTION xic_lc_guard();

CREATE TABLE IF NOT EXISTS xic_legacy_exchanges (
    entry_id           UUID         PRIMARY KEY,
    tenant_id          VARCHAR(64)  NOT NULL,
    legacy_id          UUID         NOT NULL REFERENCES xic_legacy_connectors(legacy_id),
    external_ref       VARCHAR(160) NOT NULL,
    domain_request_ref VARCHAR(128) NOT NULL DEFAULT '',
    operation          VARCHAR(80)  NOT NULL DEFAULT '',
    outcome            VARCHAR(8)   NOT NULL,
    payload_hash       VARCHAR(64)  NOT NULL,
    occurred_at        TIMESTAMPTZ  NOT NULL,
    registered_by      VARCHAR(255) NOT NULL,
    registered_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_le_outcome CHECK (outcome IN ('PENDING', 'SETTLED', 'FAILED', 'UNKNOWN')),
    CONSTRAINT xic_le_once UNIQUE (tenant_id, legacy_id, external_ref, outcome)
);
CREATE INDEX IF NOT EXISTS xic_le_ref ON xic_legacy_exchanges (legacy_id, external_ref);
CREATE INDEX IF NOT EXISTS xic_le_time ON xic_legacy_exchanges (legacy_id, occurred_at);
DROP TRIGGER IF EXISTS xic_legacy_exchanges_no_change ON xic_legacy_exchanges;
CREATE TRIGGER xic_legacy_exchanges_no_change BEFORE UPDATE OR DELETE ON xic_legacy_exchanges FOR EACH ROW EXECUTE FUNCTION xic_append_only();

CREATE TABLE IF NOT EXISTS xic_legacy_parity_runs (
    parity_id      UUID         PRIMARY KEY,
    tenant_id      VARCHAR(64)  NOT NULL,
    legacy_id      UUID         NOT NULL REFERENCES xic_legacy_connectors(legacy_id),
    window_from    TIMESTAMPTZ  NOT NULL,
    window_to      TIMESTAMPTZ  NOT NULL,
    compared       INTEGER      NOT NULL,
    matched        INTEGER      NOT NULL,
    missing_in_xic INTEGER      NOT NULL,
    outcome_differs INTEGER     NOT NULL,
    xic_only       INTEGER      NOT NULL,
    samples        JSONB        NOT NULL DEFAULT '[]',
    passed         BOOLEAN      NOT NULL,
    run_by         VARCHAR(255) NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_lp_window CHECK (window_from < window_to),
    CONSTRAINT xic_lp_counts CHECK (compared >= 0 AND matched >= 0 AND matched <= compared AND missing_in_xic >= 0 AND outcome_differs >= 0 AND xic_only >= 0)
);
CREATE INDEX IF NOT EXISTS xic_lp_legacy ON xic_legacy_parity_runs (legacy_id, created_at);
DROP TRIGGER IF EXISTS xic_legacy_parity_runs_no_change ON xic_legacy_parity_runs;
CREATE TRIGGER xic_legacy_parity_runs_no_change BEFORE UPDATE OR DELETE ON xic_legacy_parity_runs FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- ── row-level security ──────────────────────────────────────────────────────

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_rollout_requests', 'xic_rollouts', 'xic_rollout_usage', 'xic_rollout_events',
                             'xic_legacy_connectors', 'xic_legacy_exchanges', 'xic_legacy_parity_runs']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_tenant', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t || '_tenant', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_worker_read', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR SELECT
            USING (COALESCE(NULLIF(current_setting('app.xic_worker', true), ''), 'false') = 'true')$p$, t || '_worker_read', t);
    END LOOP;
END $$;
