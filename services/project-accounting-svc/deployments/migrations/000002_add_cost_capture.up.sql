-- Migration: 000002_add_cost_capture.up.sql
--
-- PRJ-02 (Project Cost Capture): "owns ProjectCostEntry. Must never own:
-- Source AP/payroll/inventory facts or duplicate source GL posting."
-- Fuller ownership (verbatim): "ProjectCostEntry; source identity;
-- project/WBS; cost type; quantity/amount/currency; billable/
-- capitalizable/WIP eligibility; source accounting reference; correction
-- chain." Purpose (verbatim): "Capture source-linked project/job cost
-- attribution from AP, expenses, payroll, inventory, assets and
-- allocations without double-posting costs already recognized by source
-- domains." The doc's own Architecture Decisions section states the
-- whole design intent in one line: "Project cost capture does not
-- double-post source costs — PRJ-02 attributes authoritative AP/payroll/
-- inventory/asset costs to projects. Only incremental reclassification/
-- capitalization effects produce new accounting events."
--
-- State model (verbatim): "Captured→Validated→Accepted→Reconciled;
-- Reclassified/Reversed via linked entries; source accounting state
-- remains separate." The same doc-vs-command mismatch pattern recurring
-- throughout this build hits PRJ-02 twice: no command reaches "Accepted"
-- independently of ValidateProjectCost — the same collapse
-- ValidateDepreciationRun already used for Run's own Calculated state —
-- and "Reconciled" has no command anywhere and is left unreachable, the
-- same posture as AST-02's own Reconciled/Certified gap. "Reclassified/
-- Reversed via linked entries" is the spec's own words for exactly the
-- append-only correction-chain pattern this session has used repeatedly
-- (AST-03's AssetEvent, INV-03's committed movements): ReclassifyProjectCost
-- and ReverseProjectCost never touch an existing entry's own economic
-- fields — each creates a brand-new entry linked back via
-- reclassifies_entry_id/reverses_entry_id.
--
-- Minimum negative-path certification (verbatim, all four):
--   1. "Same AP line captured twice to project" — per the spec's own
--      failure semantics ("Duplicate source line is idempotently
--      rejected/returned"), CaptureProjectCost is a real idempotent
--      create backed by UNIQUE(tenant_id, source_type, source_reference)
--      — a retried capture of the same source line resolves to and
--      returns the ALREADY-CAPTURED entry, the same pattern INV-03's own
--      CreateMovement uses for its own idempotency key.
--   2. "Source payroll reversal not reflected" — ReverseProjectCost is
--      the real, explicit, working command a source-domain reversal
--      event triggers, creating a linked negative-amount entry that nets
--      the original out of every aggregate query. No live payroll-run-svc
--      (or AP/INV/AST) integration exists yet to AUTOMATICALLY call it on
--      a source reversal — stated honestly as a deferred bootstrap gap,
--      the same posture PRJ-02's own Dependencies field anticipates
--      ("WFP/Payroll" does not exist as a callable service on this
--      platform).
--   3. "Project cost capture creates duplicate GL expense" — this
--      service has no general-ledger-svc client anywhere in this v1;
--      CaptureProjectCost/ValidateProjectCost/CertifyCostPopulation never
--      call any posting endpoint — structurally impossible to double-post
--      because there is no posting code path AT ALL, matching the
--      authority matrix's own words, "must never own ... duplicate source
--      GL posting."
--   4. "Manual cost amount differs from authoritative source without
--      approved adjustment" — ReclassifyProjectCost is the ONLY manual-
--      adjustment path (CaptureProjectCost is always source-driven), uses
--      its own distinct authz action (project.cost.adjust, not
--      project.cost.capture) and refuses self-approval universally (no
--      materiality tiering exists in this platform, the same bootstrap-gap
--      posture used throughout this session) — an unapproved amount
--      difference is structurally impossible to write, not merely
--      undocumented.
--
-- "project cost user cannot modify source AP/payroll/inventory facts" is
-- satisfied trivially and completely: this service's own schema has no
-- table, column, or code path that could ever write to another service's
-- own database — cross-service writes on this platform only ever happen
-- through that service's own HTTP API, and this migration defines none.
--
-- project_cost_entries is append-only exactly like AST-03's own
-- asset_events and INV-03's own committed movements: its economic fields
-- (source_type, source_reference, project_id, wbs_id, amount, currency,
-- transaction_date) are set once at INSERT and never mutated again — only
-- status, billable/capitalizable eligibility (MarkBillableEligibility)
-- and approval metadata may change in place.
CREATE TABLE project_cost_entries (
    entry_id                  UUID PRIMARY KEY,
    tenant_id                    VARCHAR(255) NOT NULL,
    legal_entity_id                 VARCHAR(255) NOT NULL,
    project_id                        UUID NOT NULL REFERENCES projects(project_id),
    wbs_id                               UUID REFERENCES project_work_packages(wbs_id),
    source_type                           VARCHAR(30) NOT NULL, -- AP|PAYROLL|INVENTORY|ASSET|ALLOCATION|MANUAL
    source_reference                        VARCHAR(255) NOT NULL,
    cost_category                             VARCHAR(100),
    quantity                                    NUMERIC(18,4),
    amount                                        NUMERIC(18,2) NOT NULL, -- may be negative for a reversal entry
    currency                                        VARCHAR(3) NOT NULL,
    transaction_date                                  DATE NOT NULL,
    billable                                            BOOLEAN NOT NULL DEFAULT FALSE,
    capitalizable                                         BOOLEAN NOT NULL DEFAULT FALSE,
    status                                                  VARCHAR(20) NOT NULL, -- CAPTURED|ACCEPTED|REVERSED (VALIDATED/RECONCILED unreachable in this v1 — see doc comment above)
    reclassifies_entry_id                                     UUID REFERENCES project_cost_entries(entry_id),
    reverses_entry_id                                           UUID REFERENCES project_cost_entries(entry_id),
    reason                                                        TEXT, -- required for reclassify/reverse
    created_at                                                      TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id                                           VARCHAR(255) NOT NULL,
    validated_at                                                        TIMESTAMP WITH TIME ZONE,
    approved_at                                                           TIMESTAMP WITH TIME ZONE, -- reclassify/reverse approval
    approved_by_principal_id                                                VARCHAR(255),

    CONSTRAINT chk_cost_entry_source_type CHECK (source_type IN ('AP', 'PAYROLL', 'INVENTORY', 'ASSET', 'ALLOCATION', 'MANUAL')),
    CONSTRAINT chk_cost_entry_status CHECK (status IN ('CAPTURED', 'ACCEPTED', 'REVERSED')),
    UNIQUE (tenant_id, source_type, source_reference)
);

-- Negative path #1/#4's own structural backbone — the economic fields
-- named in this migration's own doc comment are immutable once written;
-- only status/billable/capitalizable/approval metadata may change.
CREATE OR REPLACE FUNCTION reject_cost_entry_economic_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.source_type IS DISTINCT FROM OLD.source_type OR
       NEW.source_reference IS DISTINCT FROM OLD.source_reference OR
       NEW.project_id IS DISTINCT FROM OLD.project_id OR
       NEW.wbs_id IS DISTINCT FROM OLD.wbs_id OR
       NEW.amount IS DISTINCT FROM OLD.amount OR
       NEW.currency IS DISTINCT FROM OLD.currency OR
       NEW.transaction_date IS DISTINCT FROM OLD.transaction_date THEN
        RAISE EXCEPTION 'project_cost_entries: economic fields are immutable once written — use ReclassifyProjectCost/ReverseProjectCost, not UPDATE';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_cost_entry_economic_mutation
    BEFORE UPDATE ON project_cost_entries
    FOR EACH ROW EXECUTE FUNCTION reject_cost_entry_economic_mutation();

-- project_cost_certifications is CertifyCostPopulation's own named
-- evidence — "completion certificate" from PRJ-01's own fuller-ownership
-- field wording, applied here to a cost population snapshot.
CREATE TABLE project_cost_certifications (
    certification_id           UUID PRIMARY KEY,
    tenant_id                     VARCHAR(255) NOT NULL,
    project_id                      UUID NOT NULL REFERENCES projects(project_id),
    entry_count                       INT NOT NULL,
    total_amount                        NUMERIC(18,2) NOT NULL,
    certified_at                          TIMESTAMP WITH TIME ZONE NOT NULL,
    certified_by_principal_id               VARCHAR(255) NOT NULL
);

ALTER TABLE project_cost_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_cost_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_cost_entries
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE project_cost_certifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_cost_certifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON project_cost_certifications
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_cost_entries_project ON project_cost_entries (tenant_id, project_id);
CREATE INDEX idx_cost_entries_wbs ON project_cost_entries (tenant_id, wbs_id) WHERE wbs_id IS NOT NULL;
CREATE INDEX idx_cost_certifications_project ON project_cost_certifications (tenant_id, project_id);
