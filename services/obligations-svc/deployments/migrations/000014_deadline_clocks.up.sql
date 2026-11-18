-- Migration 000014: WF-3 Deadline Clocks (WFC-05)
-- Creates the obligation_clocks table with all clock states, escalation, and calendar provenance

CREATE TABLE obligation_clocks (
    clock_id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    obligation_id           uuid NOT NULL REFERENCES obligations(obligation_id) ON DELETE CASCADE,
    tenant_id               uuid NOT NULL,
    legal_entity_id         uuid NOT NULL,

    clock_class             text NOT NULL,  -- HARD_LEGAL, SOFT_INTERNAL, ADVISORY, EXTENSION
    clock_state             text NOT NULL DEFAULT 'PLANNED',  -- PLANNED, ACTIVE, DUE_SOON, DUE, OVERDUE, FULFILLED, WAIVED, NOT_APPLICABLE, SUPERSEDED

    -- Deadline computation provenance (jurisdiction-rules business calendar)
    calendar_code           text NOT NULL,
    calendar_rule_id        uuid NOT NULL,
    calendar_rule_version   int NOT NULL,
    computed_deadline       timestamptz NOT NULL,
    due_soon_threshold      interval NOT NULL DEFAULT '7 days',

    -- Escalation ladder (ordered steps when overdue)
    escalation_ladder       jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- Satisfaction / Waiver / N-A / Supersession audit trail
    satisfied_at            timestamptz,
    satisfied_by_principal_id text,
    satisfied_evidence_ids  uuid[] DEFAULT '{}',

    waived_at               timestamptz,
    waived_by_principal_id  text,
    waived_reason           text,
    waived_evidence_ids     uuid[] DEFAULT '{}',

    not_applicable_at       timestamptz,
    not_applicable_by_principal_id text,
    not_applicable_reason   text,

    superseded_at           timestamptz,
    superseded_by_clock_id  uuid REFERENCES obligation_clocks(clock_id),

    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    version                 int NOT NULL DEFAULT 1  -- optimistic lock for clock transitions
);

-- Indexes for common query patterns
CREATE INDEX obligation_clocks_tenant_id_idx ON obligation_clocks(tenant_id);
CREATE INDEX obligation_clocks_obligation_id_idx ON obligation_clocks(obligation_id);
CREATE INDEX obligation_clocks_legal_entity_id_idx ON obligation_clocks(legal_entity_id);
CREATE INDEX obligation_clocks_clock_state_idx ON obligation_clocks(clock_state) WHERE clock_state IN ('PLANNED', 'ACTIVE', 'DUE_SOON', 'DUE', 'OVERDUE');
CREATE INDEX obligation_clocks_computed_deadline_idx ON obligation_clocks(computed_deadline) WHERE clock_state IN ('ACTIVE', 'DUE_SOON', 'DUE');
CREATE INDEX obligation_clocks_calendar_code_idx ON obligation_clocks(calendar_code, calendar_rule_id);

-- Row-level security
ALTER TABLE obligation_clocks ENABLE ROW LEVEL SECURITY;
ALTER TABLE obligation_clocks FORCE ROW LEVEL SECURITY;

-- Policy: users can only see clocks in their tenant
CREATE POLICY obligation_clocks_tenant_isolation ON obligation_clocks
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- Trigger to update updated_at on change
CREATE TRIGGER obligation_clocks_updated_at
    BEFORE UPDATE ON obligation_clocks
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Comments
COMMENT ON TABLE obligation_clocks IS 'WF-3: Computed deadline clocks with calendar provenance, attached to obligations. States: PLANNED→ACTIVE→DUE_SOON→DUE→OVERDUE→(FULFILLED|WAIVED|NOT_APPLICABLE|SUPERSEDED).';
COMMENT ON COLUMN obligation_clocks.clock_class IS 'Clock regime: HARD_LEGAL (statutory), SOFT_INTERNAL (review), ADVISORY (info), EXTENSION (granted extension).';
COMMENT ON COLUMN obligation_clocks.clock_state IS 'Clock state machine: PLANNED, ACTIVE, DUE_SOON, DUE, OVERDUE, FULFILLED, WAIVED, NOT_APPLICABLE, SUPERSEDED.';
COMMENT ON COLUMN obligation_clocks.calendar_code IS 'Jurisdiction-rules business calendar code that produced this deadline.';
COMMENT ON COLUMN obligation_clocks.calendar_rule_id IS 'Jurisdiction-rules rule ID that produced this deadline.';
COMMENT ON COLUMN obligation_clocks.calendar_rule_version IS 'Version of the jurisdiction-rules rule at computation time.';
COMMENT ON COLUMN obligation_clocks.computed_deadline IS 'The actual computed deadline timestamp.';
COMMENT ON COLUMN obligation_clocks.due_soon_threshold IS 'Warning window before deadline (e.g. 7 days).';
COMMENT ON COLUMN obligation_clocks.escalation_ladder IS 'Ordered escalation steps when clock becomes OVERDUE.';
COMMENT ON COLUMN obligation_clocks.version IS 'Optimistic lock for clock state transitions (If-Match).';