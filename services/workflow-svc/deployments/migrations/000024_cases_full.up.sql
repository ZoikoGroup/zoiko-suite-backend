-- Migration 000024: WFC-04 Cases (Full Implementation per R-001 §6, §9, §9.1)
-- Replaces the stub 000023 with full case lifecycle, notes, members, links, transitions, work items

-- Helper function to update updated_at column
CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$;

-- Enum for case family
CREATE TYPE case_family AS ENUM ('FINANCIAL', 'PAYROLL_HR', 'LEGAL_COMPLIANCE', 'SECURITY_PRIVACY', 'OPERATIONAL');

-- Enum for case confidentiality
CREATE TYPE case_confidentiality AS ENUM ('INTERNAL', 'RESTRICTED', 'PRIVILEGED');

-- Enum for case severity
CREATE TYPE case_severity AS ENUM ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL');

-- Enum for case state (8 states per R-001 §6.3)
CREATE TYPE case_state AS ENUM ('OPEN', 'TRIAGED', 'IN_PROGRESS', 'WAITING_INTERNAL', 'WAITING_EXTERNAL', 'RESOLVED', 'CLOSED', 'REOPENED');

-- Main cases table
CREATE TABLE cases (
    case_id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    legal_entity_id         uuid NOT NULL,
    workflow_instance_id    uuid REFERENCES workflow_instances(workflow_instance_id) ON DELETE SET NULL,
    case_family             case_family NOT NULL,
    case_type               text NOT NULL,  -- INVESTIGATION, DISPUTE, EXCEPTION, etc.
    case_state              case_state NOT NULL DEFAULT 'OPEN',
    severity                case_severity NOT NULL DEFAULT 'MEDIUM',
    confidentiality         case_confidentiality NOT NULL DEFAULT 'INTERNAL',
    title                   text NOT NULL,
    description             text,
    assigned_to_principal_id text,
    created_by_principal_id  text NOT NULL,
    canonical_case_id       uuid,  -- for merge lineage
    split_from_case_id      uuid,  -- for split lineage
    opened_at               timestamptz NOT NULL DEFAULT NOW(),
    triaged_at              timestamptz,
    resolved_at             timestamptz,
    closed_at               timestamptz,
    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    row_version             int NOT NULL DEFAULT 1
);

-- Indexes
CREATE INDEX cases_tenant_id_idx ON cases(tenant_id);
CREATE INDEX cases_legal_entity_id_idx ON cases(legal_entity_id);
CREATE INDEX cases_workflow_instance_id_idx ON cases(workflow_instance_id);
CREATE INDEX cases_case_state_idx ON cases(case_state);
CREATE INDEX cases_case_type_idx ON cases(case_type);
CREATE INDEX cases_canonical_case_id_idx ON cases(canonical_case_id);
CREATE INDEX cases_split_from_case_id_idx ON cases(split_from_case_id);

-- Row-level security
ALTER TABLE cases ENABLE ROW LEVEL SECURITY;
ALTER TABLE cases FORCE ROW LEVEL SECURITY;
CREATE POLICY cases_tenant_isolation ON cases
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Trigger
CREATE TRIGGER cases_updated_at
    BEFORE UPDATE ON cases
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Case members table
CREATE TABLE case_members (
    case_member_id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    case_id                 uuid NOT NULL REFERENCES cases(case_id) ON DELETE CASCADE,
    principal_id            text NOT NULL,
    role                    text NOT NULL,  -- OWNER, ASSIGNEE, OBSERVER, etc.
    added_at                timestamptz NOT NULL DEFAULT NOW(),
    added_by_principal_id   text NOT NULL,
    removed_at              timestamptz,
    removed_by_principal_id text
);

CREATE UNIQUE INDEX case_members_unique_active ON case_members(case_id, principal_id, role) WHERE removed_at IS NULL;
CREATE INDEX case_members_tenant_id_idx ON case_members(tenant_id);
CREATE INDEX case_members_case_id_idx ON case_members(case_id);
CREATE INDEX case_members_principal_id_idx ON case_members(principal_id);

ALTER TABLE case_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE case_members FORCE ROW LEVEL SECURITY;
CREATE POLICY case_members_tenant_isolation ON case_members
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Case notes table (append-only, versioned)
CREATE TABLE case_notes (
    case_note_id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    case_id                 uuid NOT NULL REFERENCES cases(case_id) ON DELETE CASCADE,
    principal_id            text NOT NULL,
    content                 text NOT NULL,
    visibility              text NOT NULL DEFAULT 'INTERNAL',  -- INTERNAL, RESTRICTED, PRIVILEGED
    evidence_refs           text[] DEFAULT '{}',
    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    version                 int NOT NULL DEFAULT 1,
    previous_note_id        uuid REFERENCES case_notes(case_note_id) ON DELETE SET NULL
);

CREATE INDEX case_notes_tenant_id_idx ON case_notes(tenant_id);
CREATE INDEX case_notes_case_id_idx ON case_notes(case_id);
CREATE INDEX case_notes_principal_id_idx ON case_notes(principal_id);

ALTER TABLE case_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE case_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY case_notes_tenant_isolation ON case_notes
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER case_notes_updated_at
    BEFORE UPDATE ON case_notes
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Case links table (domain refs, read-only)
CREATE TABLE case_links (
    case_link_id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    case_id                 uuid NOT NULL REFERENCES cases(case_id) ON DELETE CASCADE,
    domain_object_type      text NOT NULL,  -- e.g., 'invoice', 'payroll_run', 'contract'
    domain_object_id        text NOT NULL,
    linked_at               timestamptz NOT NULL DEFAULT NOW(),
    linked_by_principal_id  text NOT NULL,
    UNIQUE (case_id, domain_object_type, domain_object_id)
);

CREATE INDEX case_links_tenant_id_idx ON case_links(tenant_id);
CREATE INDEX case_links_case_id_idx ON case_links(case_id);
CREATE INDEX case_links_domain_object_idx ON case_links(domain_object_type, domain_object_id);

ALTER TABLE case_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE case_links FORCE ROW LEVEL SECURITY;
CREATE POLICY case_links_tenant_isolation ON case_links
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Case transitions table (append-only by trigger)
CREATE TABLE case_transitions (
    case_transition_id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    case_id                 uuid NOT NULL REFERENCES cases(case_id) ON DELETE CASCADE,
    from_state              case_state,
    to_state                case_state NOT NULL,
    actor_principal_id      text NOT NULL,
    reason                  text,
    triggered_by            text NOT NULL,  -- TRIAGE, START, WAIT_INTERNAL, WAIT_EXTERNAL, RESOLVE, CLOSE, REOPEN, MERGE, SPLIT
    correlation_id          text,
    causation_id            text,
    created_at              timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX case_transitions_tenant_id_idx ON case_transitions(tenant_id);
CREATE INDEX case_transitions_case_id_idx ON case_transitions(case_id);
CREATE INDEX case_transitions_created_at_idx ON case_transitions(created_at);

ALTER TABLE case_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE case_transitions FORCE ROW LEVEL SECURITY;
CREATE POLICY case_transitions_tenant_isolation ON case_transitions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Case aliases table (for merge lineage)
CREATE TABLE case_aliases (
    alias_id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    canonical_case_id       uuid NOT NULL REFERENCES cases(case_id) ON DELETE CASCADE,
    alias_case_id           uuid NOT NULL REFERENCES cases(case_id) ON DELETE CASCADE,
    merged_at               timestamptz NOT NULL DEFAULT NOW(),
    merged_by_principal_id  text NOT NULL,
    UNIQUE (canonical_case_id, alias_case_id)
);

CREATE INDEX case_aliases_tenant_id_idx ON case_aliases(tenant_id);
CREATE INDEX case_aliases_canonical_idx ON case_aliases(canonical_case_id);
CREATE INDEX case_aliases_alias_idx ON case_aliases(alias_case_id);

ALTER TABLE case_aliases ENABLE ROW LEVEL SECURITY;
ALTER TABLE case_aliases FORCE ROW LEVEL SECURITY;
CREATE POLICY case_aliases_tenant_isolation ON case_aliases
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Generic work_items table (WFC-02 shape, extended for WF-S1 mandatory items)
CREATE TABLE work_items (
    work_item_id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    legal_entity_id         uuid NOT NULL,
    case_id                 uuid REFERENCES cases(case_id) ON DELETE SET NULL,
    workflow_instance_id    uuid REFERENCES workflow_instances(workflow_instance_id) ON DELETE SET NULL,
    item_type               text NOT NULL,  -- APPROVAL, REVIEW, INVESTIGATION, etc.
    queue                   text,
    role                    text,
    assignee_principal_id   text,
    state                   text NOT NULL DEFAULT 'UNASSIGNED',  -- UNASSIGNED, CLAIMED, COMPLETED, CANCELLED
    mandatory               boolean NOT NULL DEFAULT false,
    completion_schema       jsonb,
    due_at                  timestamptz,
    created_at              timestamptz NOT NULL DEFAULT NOW(),
    updated_at              timestamptz NOT NULL DEFAULT NOW(),
    row_version             int NOT NULL DEFAULT 1
);

CREATE INDEX work_items_tenant_id_idx ON work_items(tenant_id);
CREATE INDEX work_items_legal_entity_id_idx ON work_items(legal_entity_id);
CREATE INDEX work_items_case_id_idx ON work_items(case_id);
CREATE INDEX work_items_workflow_instance_id_idx ON work_items(workflow_instance_id);
CREATE INDEX work_items_assignee_idx ON work_items(assignee_principal_id);
CREATE INDEX work_items_state_idx ON work_items(state);

ALTER TABLE work_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_items FORCE ROW LEVEL SECURITY;
CREATE POLICY work_items_tenant_isolation ON work_items
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE TRIGGER work_items_updated_at
    BEFORE UPDATE ON work_items
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- Work item assignments history (append-only)
CREATE TABLE work_item_assignments (
    work_item_assignment_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               uuid NOT NULL,
    work_item_id            uuid NOT NULL REFERENCES work_items(work_item_id) ON DELETE CASCADE,
    previous_assignee_id    text,
    new_assignee_id         text NOT NULL,
    actor_principal_id      text NOT NULL,
    reason                  text,
    assigned_at             timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX work_item_assignments_tenant_id_idx ON work_item_assignments(tenant_id);
CREATE INDEX work_item_assignments_work_item_id_idx ON work_item_assignments(work_item_id);

ALTER TABLE work_item_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_item_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY work_item_assignments_tenant_isolation ON work_item_assignments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

COMMENT ON TABLE cases IS 'WFC-04: Cases — distinct business objects from workflows. States: OPEN→TRIAGED→IN_PROGRESS→WAITING_INTERNAL/EXTERNAL→RESOLVED/CLOSED/REOPENED.';
COMMENT ON COLUMN cases.case_family IS 'Case family: FINANCIAL, PAYROLL_HR, LEGAL_COMPLIANCE, SECURITY_PRIVACY, OPERATIONAL';
COMMENT ON COLUMN cases.case_type IS 'Case classification: INVESTIGATION, DISPUTE, EXCEPTION, etc.';
COMMENT ON COLUMN cases.case_state IS 'Case state machine: OPEN, TRIAGED, IN_PROGRESS, WAITING_INTERNAL, WAITING_EXTERNAL, RESOLVED, CLOSED, REOPENED';
COMMENT ON COLUMN cases.severity IS 'Case severity: LOW, MEDIUM, HIGH, CRITICAL';
COMMENT ON COLUMN cases.confidentiality IS 'Case confidentiality: INTERNAL, RESTRICTED, PRIVILEGED';
COMMENT ON COLUMN cases.canonical_case_id IS 'Canonical case ID for merge lineage';
COMMENT ON COLUMN cases.split_from_case_id IS 'Source case ID for split lineage';
COMMENT ON COLUMN cases.row_version IS 'Optimistic lock for case state transitions';
COMMENT ON TABLE case_members IS 'WFC-04: Case membership with roles and audit trail';
COMMENT ON TABLE case_notes IS 'WFC-04: Append-only versioned case notes with visibility and evidence refs';
COMMENT ON TABLE case_links IS 'WFC-04: Read-only links to domain objects';
COMMENT ON TABLE case_transitions IS 'WFC-04: Append-only case state transition log';
COMMENT ON TABLE case_aliases IS 'WFC-04: Merge lineage preserving source case aliases';
COMMENT ON TABLE work_items IS 'WFC-02: Generic work items for cases and workflows';
COMMENT ON TABLE work_item_assignments IS 'WFC-02: Assignment history for work items';