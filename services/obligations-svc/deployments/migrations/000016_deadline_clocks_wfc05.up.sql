-- 000016_deadline_clocks_wfc05.up.sql
-- R-001 WFC-05 (s7.1-7.4, s9, s9.1; R-INV-14..19, R-INV-26; NP-12..18, NP-32,
-- NP-37, NP-38) on top of the 19 Nov 000014 table, which had the wrong clock
-- classes, no calendar_version_id / timezone / trigger evidence, no
-- supersession link back, cascading deletes and an RLS policy without
-- WITH CHECK.
--
-- * Classes: HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY.
--   SOFT_INTERNAL becomes INTERNAL_SLA. EXTENSION is no longer a class (an
--   extension is a computation fact, extension_days_granted); an EXTENSION row
--   becomes HARD_LEGAL, the stricter regime. The old value is kept in
--   legacy_clock_class so the down migration restores it.
-- * Provenance (R-INV-16): every field jurisdiction-rules-svc's
--   POST /v1/obligation-calculations:calculate returns is stored verbatim.
-- * Trigger evidence (s7.1, NP-12): event id and time, business event time,
--   source object and version, ingestion time and latency.
-- * due_at and its provenance never change in place (R-INV-17): a trigger
--   refuses the update; a recalculation creates a successor clock.
-- * Satisfaction (R-INV-14), waiver / not-applicable with authority
--   (s7.1, NP-18), lateness kept after fulfilment (NP-38).
-- * Milestones (warning schedule independent from due_at), append-only
--   transitions, versioned escalation policies and append-only escalation
--   events (s9).
-- * Common columns (s9.1); FORCE RLS with WITH CHECK on every table; no
--   ON DELETE CASCADE.

BEGIN;

-- ── obligation_clocks: classes ──────────────────────────────────────────────
ALTER TABLE obligation_clocks ADD COLUMN legacy_clock_class text;
UPDATE obligation_clocks
   SET legacy_clock_class = clock_class,
       clock_class = CASE clock_class WHEN 'SOFT_INTERNAL' THEN 'INTERNAL_SLA' ELSE 'HARD_LEGAL' END
 WHERE clock_class IN ('SOFT_INTERNAL', 'EXTENSION');
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_class_chk
    CHECK (clock_class IN ('HARD_LEGAL', 'REGULATORY', 'CONTRACTUAL', 'INTERNAL_SLA', 'ADVISORY'));
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_state_chk
    CHECK (clock_state IN ('PLANNED', 'ACTIVE', 'DUE_SOON', 'DUE', 'OVERDUE', 'FULFILLED', 'WAIVED', 'NOT_APPLICABLE', 'SUPERSEDED'));

-- ── renames to the R-001 s9 names ───────────────────────────────────────────
ALTER TABLE obligation_clocks RENAME COLUMN computed_deadline TO due_at;
ALTER TABLE obligation_clocks RENAME COLUMN calendar_rule_id TO obligation_rule_id;
ALTER TABLE obligation_clocks ALTER COLUMN obligation_rule_id TYPE text USING obligation_rule_id::text;
ALTER TABLE obligation_clocks RENAME COLUMN calendar_rule_version TO rule_version;
ALTER TABLE obligation_clocks RENAME COLUMN version TO row_version;
ALTER TABLE obligation_clocks RENAME COLUMN escalation_ladder TO legacy_escalation_ladder;
ALTER TABLE obligation_clocks RENAME COLUMN satisfied_evidence_ids TO legacy_satisfied_evidence_ids;
ALTER TABLE obligation_clocks RENAME COLUMN waived_evidence_ids TO legacy_waived_evidence_ids;
ALTER TABLE obligation_clocks ALTER COLUMN calendar_code DROP NOT NULL;

-- ── provenance (R-INV-16) ────────────────────────────────────────────────────
ALTER TABLE obligation_clocks
    ADD COLUMN clock_key               text,
    ADD COLUMN request_hash            text,
    ADD COLUMN calculation_request     jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN jurisdiction_code       text,
    ADD COLUMN rule_obligation_code    text,
    ADD COLUMN rule_content_digest     text,
    ADD COLUMN calendar_version        int,
    ADD COLUMN calendar_version_id     text,
    ADD COLUMN timezone                text,
    ADD COLUMN business_day_adjustment text,
    ADD COLUMN anchor_date             date,
    ADD COLUMN unadjusted_date         date,
    ADD COLUMN local_due_date          date,
    ADD COLUMN extension_days_granted  int NOT NULL DEFAULT 0,
    ADD COLUMN pack_ref                text,
    ADD COLUMN pack_version            text,
    ADD COLUMN pack_version_id         text,
    ADD COLUMN calculation_ref         text,
    ADD COLUMN calculation_explanation jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- trigger evidence (s7.1, NP-12)
    ADD COLUMN trigger_event_id        text,
    ADD COLUMN trigger_event_type      text,
    ADD COLUMN trigger_event_time      timestamptz,
    ADD COLUMN business_event_time     timestamptz,
    ADD COLUMN source_object_type      text,
    ADD COLUMN source_object_id        text,
    ADD COLUMN source_object_version   text,
    ADD COLUMN ingested_at             timestamptz NOT NULL DEFAULT NOW(),
    ADD COLUMN ingestion_latency_ms    bigint,
    -- supersession (R-INV-17, NP-14, NP-32)
    ADD COLUMN supersedes_clock_id     uuid REFERENCES obligation_clocks (clock_id),
    ADD COLUMN supersession_reason     text,
    -- satisfaction (R-INV-14)
    ADD COLUMN evidence_refs           text[] NOT NULL DEFAULT '{}',
    ADD COLUMN evidence_evaluation_id  text,
    ADD COLUMN evidence_outcome        text,
    ADD COLUMN evidence_exempt_reason  text,
    ADD COLUMN domain_confirmation_ref text,
    ADD COLUMN subject_state           text,
    -- waiver / not applicable with authority (s7.1, NP-18)
    ADD COLUMN authority_action        text,
    ADD COLUMN requested_by            text,
    ADD COLUMN approved_by             text,
    ADD COLUMN approval_reference      text,
    ADD COLUMN reason_code             text,
    ADD COLUMN reason_text             text,
    ADD COLUMN authority_evidence_refs text[] NOT NULL DEFAULT '{}',
    ADD COLUMN applicability_decision_id uuid,
    -- lateness (NP-38): set once the clock is late, never cleared
    ADD COLUMN overdue_since           timestamptz,
    -- escalation (s7.4): priority and owners only; never the clock's meaning
    ADD COLUMN escalation_policy_id    uuid,
    ADD COLUMN escalation_policy_version int,
    ADD COLUMN priority                text NOT NULL DEFAULT 'NORMAL',
    ADD COLUMN additional_owners       text[] NOT NULL DEFAULT '{}',
    -- common columns (s9.1)
    ADD COLUMN plane                   text NOT NULL DEFAULT 'TENANT_BUSINESS',
    ADD COLUMN correlation_id          text,
    ADD COLUMN causation_id            text,
    ADD COLUMN data_class              text NOT NULL DEFAULT 'CONFIDENTIAL',
    ADD COLUMN residency_region        text,
    ADD COLUMN created_by              text,
    ADD COLUMN updated_by              text;

-- Carry the 19 Nov columns into the new ones.
UPDATE obligation_clocks SET
    clock_key = clock_id::text,
    evidence_refs = COALESCE(ARRAY(SELECT unnest(legacy_satisfied_evidence_ids)::text), '{}'),
    authority_action = CASE WHEN waived_at IS NOT NULL THEN 'WAIVE' WHEN not_applicable_at IS NOT NULL THEN 'NOT_APPLICABLE' END,
    requested_by = COALESCE(waived_by_principal_id, not_applicable_by_principal_id),
    reason_text = COALESCE(waived_reason, not_applicable_reason),
    authority_evidence_refs = COALESCE(ARRAY(SELECT unnest(legacy_waived_evidence_ids)::text), '{}'),
    created_by = 'legacy-000014';
ALTER TABLE obligation_clocks ALTER COLUMN clock_key SET NOT NULL;
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_key_uq UNIQUE (tenant_id, clock_key);

ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_authority_chk CHECK (
    authority_action IS NULL OR authority_action IN ('WAIVE', 'NOT_APPLICABLE'));
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_priority_chk CHECK (
    priority IN ('NORMAL', 'HIGH', 'CRITICAL'));
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_plane_chk CHECK (
    plane IN ('TENANT_BUSINESS', 'PLATFORM_COMMERCIAL', 'PLATFORM_CONTROL'));
-- R-INV-14: FULFILLED carries evidence or an approved exemption reason.
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_fulfilled_evidence_chk CHECK (
    clock_state <> 'FULFILLED' OR cardinality(evidence_refs) > 0 OR evidence_exempt_reason IS NOT NULL
    OR created_by = 'legacy-000014') NOT VALID;
-- s7.1: a waiver or N/A names the requester, a different approver and a reason.
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_authority_fields_chk CHECK (
    clock_state NOT IN ('WAIVED', 'NOT_APPLICABLE') OR created_by = 'legacy-000014' OR (
        authority_action IS NOT NULL AND requested_by IS NOT NULL AND approved_by IS NOT NULL
        AND approved_by <> requested_by AND reason_code IS NOT NULL AND approval_reference IS NOT NULL));
-- A NOT_APPLICABLE clock links the applicability decision that resolved it.
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_na_decision_chk CHECK (
    clock_state <> 'NOT_APPLICABLE' OR applicability_decision_id IS NOT NULL OR created_by = 'legacy-000014');

-- No cascading delete: a clock is evidence and outlives nothing by accident.
ALTER TABLE obligation_clocks DROP CONSTRAINT IF EXISTS obligation_clocks_obligation_id_fkey;
ALTER TABLE obligation_clocks ADD CONSTRAINT obligation_clocks_obligation_id_fkey
    FOREIGN KEY (obligation_id) REFERENCES obligations (obligation_id);

CREATE INDEX IF NOT EXISTS obligation_clocks_due_idx ON obligation_clocks (due_at)
    WHERE clock_state IN ('PLANNED', 'ACTIVE', 'DUE_SOON', 'DUE', 'OVERDUE');
CREATE INDEX IF NOT EXISTS obligation_clocks_calendar_version_idx ON obligation_clocks (calendar_code, calendar_version)
    WHERE clock_state IN ('PLANNED', 'ACTIVE', 'DUE_SOON', 'DUE', 'OVERDUE');
CREATE INDEX IF NOT EXISTS obligation_clocks_page_idx ON obligation_clocks (tenant_id, created_at, clock_id);

-- R-INV-17: due_at and the provenance that produced it never change in place.
-- A recalculation supersedes the clock and creates a successor.
CREATE OR REPLACE FUNCTION obligation_clocks_immutable_provenance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.due_at IS DISTINCT FROM OLD.due_at
       OR NEW.clock_class IS DISTINCT FROM OLD.clock_class
       OR NEW.obligation_rule_id IS DISTINCT FROM OLD.obligation_rule_id
       OR NEW.rule_version IS DISTINCT FROM OLD.rule_version
       OR NEW.rule_content_digest IS DISTINCT FROM OLD.rule_content_digest
       OR NEW.calendar_code IS DISTINCT FROM OLD.calendar_code
       OR NEW.calendar_version IS DISTINCT FROM OLD.calendar_version
       OR NEW.calendar_version_id IS DISTINCT FROM OLD.calendar_version_id
       OR NEW.timezone IS DISTINCT FROM OLD.timezone
       OR NEW.local_due_date IS DISTINCT FROM OLD.local_due_date
       OR NEW.trigger_event_id IS DISTINCT FROM OLD.trigger_event_id
       OR NEW.trigger_event_time IS DISTINCT FROM OLD.trigger_event_time
       OR NEW.business_event_time IS DISTINCT FROM OLD.business_event_time
       OR NEW.obligation_id IS DISTINCT FROM OLD.obligation_id
       OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.clock_key IS DISTINCT FROM OLD.clock_key THEN
        RAISE EXCEPTION 'obligation_clocks: due_at and its provenance are immutable (R-INV-17); supersede the clock instead'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.overdue_since IS NOT NULL AND NEW.overdue_since IS DISTINCT FROM OLD.overdue_since THEN
        RAISE EXCEPTION 'obligation_clocks: overdue_since is kept once set (NP-38)' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.clock_state IN ('FULFILLED', 'WAIVED', 'NOT_APPLICABLE', 'SUPERSEDED')
       AND NEW.clock_state IS DISTINCT FROM OLD.clock_state THEN
        RAISE EXCEPTION 'obligation_clocks: % is terminal', OLD.clock_state USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER obligation_clocks_immutable_provenance BEFORE UPDATE ON obligation_clocks
    FOR EACH ROW EXECUTE FUNCTION obligation_clocks_immutable_provenance();

CREATE OR REPLACE FUNCTION obligations_refuse_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%: rows are never deleted; use a state transition', TG_TABLE_NAME USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER obligation_clocks_no_delete BEFORE DELETE ON obligation_clocks
    FOR EACH ROW EXECUTE FUNCTION obligations_refuse_delete();

DROP POLICY IF EXISTS obligation_clocks_tenant_isolation ON obligation_clocks;
CREATE POLICY obligation_clocks_tenant_isolation ON obligation_clocks FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
-- The sweep and the calendar consumer find work across tenants with this
-- read-only policy, then act on each clock inside that clock's tenant.
CREATE POLICY obligation_clocks_sweep_select ON obligation_clocks FOR SELECT
    USING (current_setting('app.clock_sweep', true) = 'true');

-- ── escalation policies (versioned, s9) ──────────────────────────────────────
CREATE TABLE escalation_policies (
    escalation_policy_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            uuid NOT NULL,
    policy_code          text NOT NULL,
    version              int  NOT NULL CHECK (version > 0),
    status               text NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'RETIRED')),
    -- [{kind, offset_seconds, action, params}]; offset relative to due_at.
    milestones           jsonb NOT NULL CHECK (jsonb_typeof(milestones) = 'array'),
    effective_from       timestamptz NOT NULL DEFAULT NOW(),
    effective_to         timestamptz,
    plane                text NOT NULL DEFAULT 'TENANT_BUSINESS',
    data_class           text NOT NULL DEFAULT 'INTERNAL',
    residency_region     text,
    correlation_id       text,
    created_by           text NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT NOW(),
    row_version          int NOT NULL DEFAULT 1,
    UNIQUE (tenant_id, policy_code, version)
);
CREATE OR REPLACE FUNCTION escalation_policies_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.milestones IS DISTINCT FROM OLD.milestones OR NEW.policy_code IS DISTINCT FROM OLD.policy_code
       OR NEW.version IS DISTINCT FROM OLD.version OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.effective_from IS DISTINCT FROM OLD.effective_from THEN
        RAISE EXCEPTION 'escalation_policies: a version is immutable; create a new version' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER escalation_policies_immutable BEFORE UPDATE ON escalation_policies
    FOR EACH ROW EXECUTE FUNCTION escalation_policies_immutable();
CREATE TRIGGER escalation_policies_no_delete BEFORE DELETE ON escalation_policies
    FOR EACH ROW EXECUTE FUNCTION obligations_refuse_delete();

-- ── milestones: warning schedule independent from due_at (s7.1) ──────────────
CREATE TABLE obligation_clock_milestones (
    milestone_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL,
    clock_id         uuid NOT NULL REFERENCES obligation_clocks (clock_id),
    kind             text NOT NULL CHECK (kind IN ('WARNING', 'ESCALATION')),
    offset_seconds   bigint NOT NULL,
    fire_at          timestamptz NOT NULL,
    action           text NOT NULL CHECK (action IN ('MARK_DUE_SOON', 'RAISE_PRIORITY', 'ADD_OWNER', 'OPEN_CASE', 'NOTIFY', 'REQUIRE_APPROVAL')),
    action_params    jsonb NOT NULL DEFAULT '{}'::jsonb,
    escalation_policy_id uuid REFERENCES escalation_policies (escalation_policy_id),
    escalation_policy_version int,
    fired_at         timestamptz,
    created_at       timestamptz NOT NULL DEFAULT NOW(),
    UNIQUE (clock_id, kind, offset_seconds, action)
);
CREATE INDEX obligation_clock_milestones_due_idx ON obligation_clock_milestones (fire_at) WHERE fired_at IS NULL;
CREATE OR REPLACE FUNCTION obligation_clock_milestones_fire_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.fired_at IS NOT NULL OR NEW.clock_id IS DISTINCT FROM OLD.clock_id OR NEW.fire_at IS DISTINCT FROM OLD.fire_at
       OR NEW.action IS DISTINCT FROM OLD.action OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
        RAISE EXCEPTION 'obligation_clock_milestones: a milestone fires once and is otherwise immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER obligation_clock_milestones_fire_once BEFORE UPDATE ON obligation_clock_milestones
    FOR EACH ROW EXECUTE FUNCTION obligation_clock_milestones_fire_once();
CREATE TRIGGER obligation_clock_milestones_no_delete BEFORE DELETE ON obligation_clock_milestones
    FOR EACH ROW EXECUTE FUNCTION obligations_refuse_delete();

-- ── transitions: append-only history (NP-32, NP-38, NP-39) ────────────────────
CREATE TABLE obligation_clock_transitions (
    transition_id    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL,
    clock_id         uuid NOT NULL REFERENCES obligation_clocks (clock_id),
    from_state       text,
    to_state         text NOT NULL,
    command          text NOT NULL,
    actor            text NOT NULL,
    reason_code      text,
    reason_text      text,
    evidence_refs    text[] NOT NULL DEFAULT '{}',
    row_version      int NOT NULL,
    -- the logical instant of the transition (due_at for DUE and OVERDUE),
    -- which a late sweep must not move (R-INV-26)
    effective_at     timestamptz NOT NULL,
    recorded_at      timestamptz NOT NULL DEFAULT NOW(),
    correlation_id   text,
    causation_id     text
);
CREATE INDEX obligation_clock_transitions_clock_idx ON obligation_clock_transitions (clock_id, recorded_at, transition_id);
-- One persisted move per (clock, row_version): a restarted sweep cannot write
-- the same transition twice (NP-37).
CREATE UNIQUE INDEX obligation_clock_transitions_once ON obligation_clock_transitions (clock_id, row_version);

CREATE OR REPLACE FUNCTION obligations_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%: append-only', TG_TABLE_NAME USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER obligation_clock_transitions_append_only BEFORE UPDATE OR DELETE ON obligation_clock_transitions
    FOR EACH ROW EXECUTE FUNCTION obligations_append_only();

-- ── escalation events (append-only, s9) ──────────────────────────────────────
CREATE TABLE escalation_events (
    escalation_event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        uuid NOT NULL,
    clock_id         uuid NOT NULL REFERENCES obligation_clocks (clock_id),
    milestone_id     uuid REFERENCES obligation_clock_milestones (milestone_id),
    action           text NOT NULL,
    outcome          text NOT NULL CHECK (outcome IN ('REQUESTED', 'APPLIED', 'COMPLETED', 'FAILED', 'SKIPPED')),
    result_ref       text,
    detail           jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence_refs    text[] NOT NULL DEFAULT '{}',
    occurred_at      timestamptz NOT NULL DEFAULT NOW(),
    correlation_id   text
);
CREATE INDEX escalation_events_clock_idx ON escalation_events (clock_id, occurred_at);
CREATE TRIGGER escalation_events_append_only BEFORE UPDATE OR DELETE ON escalation_events
    FOR EACH ROW EXECUTE FUNCTION obligations_append_only();

-- ── RLS on the new tables ────────────────────────────────────────────────────
ALTER TABLE escalation_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE escalation_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON escalation_policies FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE obligation_clock_milestones ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligation_clock_milestones FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON obligation_clock_milestones FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY milestones_sweep_select ON obligation_clock_milestones FOR SELECT
    USING (current_setting('app.clock_sweep', true) = 'true');

ALTER TABLE obligation_clock_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligation_clock_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON obligation_clock_transitions FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE escalation_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE escalation_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON escalation_events FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- ── outbox destinations ──────────────────────────────────────────────────────
-- An escalation that opens a workflow-svc case is delivered by the same
-- outbox as the events, under its own destination, so the case request
-- commits with the clock change and is retried until workflow-svc accepts it.
ALTER TABLE event_outbox ADD COLUMN destination text NOT NULL DEFAULT 'KAFKA'
    CHECK (destination IN ('KAFKA', 'WORKFLOW_CASE'));
CREATE INDEX IF NOT EXISTS idx_event_outbox_destination_due ON event_outbox (destination, status, next_attempt_at)
    WHERE status = 'PENDING';

-- ── calendar events already applied (replay-safe consumer, OB-S8) ────────────
CREATE TABLE consumed_events (
    event_id     text PRIMARY KEY,
    event_type   text NOT NULL,
    consumed_at  timestamptz NOT NULL DEFAULT NOW(),
    outcome      jsonb NOT NULL DEFAULT '{}'::jsonb
);
-- Calendar events are platform-wide (no tenant); the consumer is a system
-- worker. The table holds no tenant data, so RLS is not needed; it is still
-- enabled and gated on the sweep flag so a tenant session cannot read it.
ALTER TABLE consumed_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE consumed_events FORCE ROW LEVEL SECURITY;
CREATE POLICY consumed_events_worker ON consumed_events FOR ALL
    USING (current_setting('app.clock_sweep', true) = 'true')
    WITH CHECK (current_setting('app.clock_sweep', true) = 'true');

COMMIT;
