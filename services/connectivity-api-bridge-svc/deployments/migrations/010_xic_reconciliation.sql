-- 010 ZS-SVC-T-001 XIC-05 reconciliation: independent external-vs-internal comparison runs and their variances.
--
-- A run compares what the provider declares (control counts, references, totals, receipts, manifests) with what the
-- platform and the owning domain hold. Reconciliation never rewrites exchange history: it records a run and opens
-- variances, each with a case/exception path (§10.4, I-25, I-26).
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd) on every table;
--   * a run's identity, period, basis, external declaration and requester never change; its state moves only along
--     OPEN -> MATCHED | VARIANCE | ESCALATED -> CLOSED, and CLOSED is final;
--   * the comparison is independent: a run that compares counts, references or amounts holds an external declaration
--     from a named source, and whoever supplied the domain's totals is not whoever supplied the provider's;
--   * a variance's identity, type, run and evidence never change; an open variance is held once per (connection,
--     fingerprint), so a repeat is counted, not duplicated; RESOLVED and ACCEPTED are final, an accepted difference
--     carries a case reference and a note, and nothing here ever edits an exchange, attempt, batch or receipt;
--   * evidence is append-only.

CREATE TABLE IF NOT EXISTS xic_reconciliation_runs (
    run_id               UUID         PRIMARY KEY,
    tenant_id            VARCHAR(64)  NOT NULL,
    connection_id        UUID         NOT NULL REFERENCES xic_connections(connection_id),
    connector_id         VARCHAR(63)  NOT NULL,
    family               VARCHAR(24)  NOT NULL,
    origin               VARCHAR(9)   NOT NULL,
    period_from          TIMESTAMPTZ  NOT NULL,
    period_to            TIMESTAMPTZ  NOT NULL,
    basis                TEXT[]       NOT NULL,
    state                VARCHAR(9)   NOT NULL DEFAULT 'OPEN',
    external_source      VARCHAR(24)  NOT NULL DEFAULT '',
    external             JSONB,
    external_supplied_by VARCHAR(255) NOT NULL DEFAULT '',
    awaiting_domain      BOOLEAN      NOT NULL DEFAULT false,
    domain_totals        JSONB,
    domain_totals_by     VARCHAR(255) NOT NULL DEFAULT '',
    dedupe_key           VARCHAR(64)  NOT NULL,
    result               JSONB,
    variance_count       INTEGER      NOT NULL DEFAULT 0,
    open_variances       INTEGER      NOT NULL DEFAULT 0,
    high_variances       INTEGER      NOT NULL DEFAULT 0,
    evaluated_at         TIMESTAMPTZ,
    next_evaluate_at     TIMESTAMPTZ,
    lease_until          TIMESTAMPTZ,
    closed_by            VARCHAR(255) NOT NULL DEFAULT '',
    close_note           VARCHAR(1000) NOT NULL DEFAULT '',
    requested_by         VARCHAR(255) NOT NULL,
    correlation_id       VARCHAR(128) NOT NULL DEFAULT '',
    version              BIGINT       NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_rr_dedupe UNIQUE (tenant_id, dedupe_key),
    CONSTRAINT xic_rr_state_valid CHECK (state IN ('OPEN', 'MATCHED', 'VARIANCE', 'ESCALATED', 'CLOSED')),
    CONSTRAINT xic_rr_origin_valid CHECK (origin IN ('REQUESTED', 'SCHEDULED')),
    CONSTRAINT xic_rr_period CHECK (period_from < period_to),
    CONSTRAINT xic_rr_basis CHECK (cardinality(basis) >= 1 AND basis <@ ARRAY['COUNT', 'REFERENCE', 'AMOUNT', 'STATE', 'EVIDENCE', 'TIME']::text[]),
    CONSTRAINT xic_rr_counts CHECK (variance_count >= 0 AND open_variances >= 0 AND high_variances >= 0 AND open_variances <= variance_count AND version >= 1),
    -- NP-44: a comparison of counts, references, amounts or time needs a declaration that does not come from the platform
    CONSTRAINT xic_rr_independent CHECK (NOT (basis && ARRAY['COUNT', 'REFERENCE', 'AMOUNT', 'TIME']::text[])
        OR (external IS NOT NULL AND external_source <> '' AND external_source NOT IN ('INTERNAL', 'XIC') AND external_supplied_by <> '')),
    CONSTRAINT xic_rr_distinct_suppliers CHECK (domain_totals_by = '' OR domain_totals_by <> external_supplied_by),
    CONSTRAINT xic_rr_domain_totals CHECK ((domain_totals IS NULL) = (domain_totals_by = '')),
    CONSTRAINT xic_rr_evaluated CHECK (state = 'OPEN' OR evaluated_at IS NOT NULL),
    CONSTRAINT xic_rr_closed_by CHECK (state <> 'CLOSED' OR closed_by <> '')
);
CREATE INDEX IF NOT EXISTS xic_rr_due ON xic_reconciliation_runs (next_evaluate_at) WHERE state = 'OPEN';
CREATE INDEX IF NOT EXISTS xic_rr_conn ON xic_reconciliation_runs (connection_id, created_at);

CREATE OR REPLACE FUNCTION xic_rr_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a reconciliation run is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.run_id <> OLD.run_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.connection_id <> OLD.connection_id OR NEW.connector_id <> OLD.connector_id
       OR NEW.family <> OLD.family OR NEW.origin <> OLD.origin OR NEW.period_from <> OLD.period_from OR NEW.period_to <> OLD.period_to
       OR NEW.basis <> OLD.basis OR NEW.external_source <> OLD.external_source OR NEW.external::text IS DISTINCT FROM OLD.external::text
       OR NEW.external_supplied_by <> OLD.external_supplied_by OR NEW.dedupe_key <> OLD.dedupe_key OR NEW.requested_by <> OLD.requested_by
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity, period, basis and external declaration of a reconciliation run are fixed at request' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'reconciliation run version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.domain_totals IS NOT NULL AND (NEW.domain_totals::text IS DISTINCT FROM OLD.domain_totals::text OR NEW.domain_totals_by <> OLD.domain_totals_by) THEN
        RAISE EXCEPTION 'the domain totals of a run are supplied once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'CLOSED' THEN
        RAISE EXCEPTION 'a closed reconciliation run is final' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> NEW.state AND NOT (
           (OLD.state = 'OPEN'      AND NEW.state IN ('MATCHED', 'VARIANCE', 'ESCALATED'))
        OR (OLD.state = 'MATCHED'   AND NEW.state = 'CLOSED')
        OR (OLD.state = 'VARIANCE'  AND NEW.state IN ('ESCALATED', 'CLOSED'))
        OR (OLD.state = 'ESCALATED' AND NEW.state = 'CLOSED')) THEN
        RAISE EXCEPTION 'reconciliation state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- a run is closed only when none of its variances is still open
    IF NEW.state = 'CLOSED' AND NEW.open_variances > 0 THEN
        RAISE EXCEPTION 'a run with open variances is not closed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_rr_guard_trg ON xic_reconciliation_runs;
CREATE TRIGGER xic_rr_guard_trg BEFORE UPDATE OR DELETE ON xic_reconciliation_runs FOR EACH ROW EXECUTE FUNCTION xic_rr_guard();

CREATE TABLE IF NOT EXISTS xic_reconciliation_variances (
    variance_id       UUID         PRIMARY KEY,
    tenant_id         VARCHAR(64)  NOT NULL,
    run_id            UUID         NOT NULL REFERENCES xic_reconciliation_runs(run_id),
    last_run_id       UUID         NOT NULL REFERENCES xic_reconciliation_runs(run_id),
    connection_id     UUID         NOT NULL REFERENCES xic_connections(connection_id),
    variance_type     VARCHAR(32)  NOT NULL,
    severity          VARCHAR(6)   NOT NULL,
    state             VARCHAR(9)   NOT NULL DEFAULT 'OPEN',
    fingerprint       VARCHAR(64)  NOT NULL,
    refs              JSONB        NOT NULL,
    repeat_count      INTEGER      NOT NULL DEFAULT 1,
    case_ref          VARCHAR(255) NOT NULL DEFAULT '',
    resolution        VARCHAR(24)  NOT NULL DEFAULT '',
    resolution_note   VARCHAR(1000) NOT NULL DEFAULT '',
    resolved_by       VARCHAR(255) NOT NULL DEFAULT '',
    resolved_at       TIMESTAMPTZ,
    escalated_at      TIMESTAMPTZ,
    version           BIGINT       NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_rv_type_valid CHECK (variance_type IN ('COUNT_MISMATCH', 'REFERENCE_MISSING_INTERNAL', 'REFERENCE_MISSING_EXTERNAL', 'AMOUNT_MISMATCH', 'STATE_MISMATCH',
        'UNRESOLVED_UNKNOWN', 'STALE_PENDING', 'LATE_CALLBACK_CONTRADICTION', 'MANIFEST_VARIANCE', 'BATCH_INCOMPLETE', 'LATE_ARRIVAL')),
    CONSTRAINT xic_rv_severity_valid CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH')),
    CONSTRAINT xic_rv_state_valid CHECK (state IN ('OPEN', 'ESCALATED', 'RESOLVED', 'ACCEPTED')),
    CONSTRAINT xic_rv_fingerprint CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_rv_repeat CHECK (repeat_count >= 1 AND version >= 1),
    -- an accepted difference is a decision with a reason and a case; a resolved variance names how
    CONSTRAINT xic_rv_accepted_reasoned CHECK (state <> 'ACCEPTED' OR (btrim(case_ref) <> '' AND btrim(resolution_note) <> '' AND resolved_by <> '')),
    CONSTRAINT xic_rv_resolved_named CHECK (state <> 'RESOLVED' OR (resolution <> '' AND resolved_by <> '' AND resolved_at IS NOT NULL)),
    CONSTRAINT xic_rv_escalated_stamped CHECK (state <> 'ESCALATED' OR escalated_at IS NOT NULL)
);
-- an open variance is held once: a repeat finding of the same difference counts, it is not a second variance
CREATE UNIQUE INDEX IF NOT EXISTS xic_rv_open_once ON xic_reconciliation_variances (tenant_id, connection_id, fingerprint) WHERE state IN ('OPEN', 'ESCALATED');
CREATE INDEX IF NOT EXISTS xic_rv_run ON xic_reconciliation_variances (run_id);
CREATE INDEX IF NOT EXISTS xic_rv_open ON xic_reconciliation_variances (tenant_id, created_at) WHERE state IN ('OPEN', 'ESCALATED');

CREATE OR REPLACE FUNCTION xic_rv_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a reconciliation variance is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.variance_id <> OLD.variance_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.run_id <> OLD.run_id OR NEW.connection_id <> OLD.connection_id
       OR NEW.variance_type <> OLD.variance_type OR NEW.fingerprint <> OLD.fingerprint OR NEW.refs::text <> OLD.refs::text OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity, type and evidence of a variance are fixed when it is opened' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'variance version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('RESOLVED', 'ACCEPTED') THEN
        RAISE EXCEPTION 'a % variance is final', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> NEW.state AND NOT (
           (OLD.state = 'OPEN'      AND NEW.state IN ('ESCALATED', 'RESOLVED', 'ACCEPTED'))
        OR (OLD.state = 'ESCALATED' AND NEW.state IN ('RESOLVED', 'ACCEPTED'))) THEN
        RAISE EXCEPTION 'variance state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.repeat_count < OLD.repeat_count THEN
        RAISE EXCEPTION 'a variance repeat count only grows' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.severity = 'HIGH' AND NEW.severity <> 'HIGH' THEN
        RAISE EXCEPTION 'a variance does not become less severe than it was found' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_rv_guard_trg ON xic_reconciliation_variances;
CREATE TRIGGER xic_rv_guard_trg BEFORE UPDATE OR DELETE ON xic_reconciliation_variances FOR EACH ROW EXECUTE FUNCTION xic_rv_guard();

CREATE TABLE IF NOT EXISTS xic_reconciliation_events (
    event_id    UUID         PRIMARY KEY,
    tenant_id   VARCHAR(64)  NOT NULL,
    run_id      UUID         NOT NULL REFERENCES xic_reconciliation_runs(run_id),
    variance_id UUID         REFERENCES xic_reconciliation_variances(variance_id),
    kind        VARCHAR(32)  NOT NULL,
    actor_id    VARCHAR(255) NOT NULL DEFAULT '',
    detail      JSONB,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS xic_re_run ON xic_reconciliation_events (run_id, created_at);
DROP TRIGGER IF EXISTS xic_reconciliation_events_no_change ON xic_reconciliation_events;
CREATE TRIGGER xic_reconciliation_events_no_change BEFORE UPDATE OR DELETE ON xic_reconciliation_events FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- ── row-level security ──────────────────────────────────────────────────────

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_reconciliation_runs', 'xic_reconciliation_variances', 'xic_reconciliation_events']
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
