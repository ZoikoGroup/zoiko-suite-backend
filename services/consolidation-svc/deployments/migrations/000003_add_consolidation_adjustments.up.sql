-- Migration: 000003_add_consolidation_adjustments.up.sql
--
-- ACC-12 (Elimination & Consolidation Adjustments): "owns Consolidation
-- adjustment lifecycle. Must never own: Entity statutory ledgers." Fuller
-- ownership: "ConsolidationAdjustment, elimination rule version, approval
-- state and consolidation-book journal ref." State model (verbatim):
-- "Draft → PendingApproval → Approved → Posted → Reversed/Superseded."
--
-- Distinct from StartRun's own automatic elimination step
-- (handler.go's eliminateMatchedEntry, see master-register-findings
-- §3.29): that path only ever adjusts one run's in-memory snapshot math,
-- is never approved or posted, and touches no ledger. A
-- ConsolidationAdjustment is a governed, human-approved journal that
-- actually posts to general-ledger-svc under the group entity via ACC-04's
-- PostAccountingEvent (the system-originated posting path already built
-- for exactly this kind of system-to-system entry) — real "ACC-04/05
-- consolidation book" dependency, not a simulated one.
--
-- No separate "Submit" command is named anywhere in the wireframe or
-- contract table (only Create/Approve/Post/Reverse), so this v1 has no
-- DRAFT resting state reachable via the API — CreateEliminationProposal
-- lands directly in PENDING_APPROVAL, the same honest gap this register
-- already noted for ACC-03's own missing Cancel command.
CREATE TABLE consolidation_adjustments (
    consolidation_adjustment_id   UUID PRIMARY KEY,
    tenant_id                     VARCHAR(255) NOT NULL,
    group_legal_entity_id         VARCHAR(255) NOT NULL,
    fiscal_period                 VARCHAR(50) NOT NULL,
    adjustment_type               VARCHAR(20) NOT NULL, -- ELIMINATION | MANUAL
    description                   TEXT,
    status                        VARCHAR(20) NOT NULL, -- PENDING_APPROVAL | APPROVED | POSTED | REVERSED
    lines                         JSONB NOT NULL,

    consolidation_book_journal_id UUID,

    created_at                    TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id       VARCHAR(255) NOT NULL,
    approved_at                   TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id      VARCHAR(255),
    posted_at                     TIMESTAMP WITH TIME ZONE,
    posted_by_principal_id        VARCHAR(255),
    reversed_at                   TIMESTAMP WITH TIME ZONE,
    reversed_by_principal_id      VARCHAR(255),
    reversal_reason               TEXT,
    superseded_by_adjustment_id   UUID REFERENCES consolidation_adjustments(consolidation_adjustment_id),

    CONSTRAINT chk_adjustment_type CHECK (adjustment_type IN ('ELIMINATION', 'MANUAL')),
    CONSTRAINT chk_adjustment_status CHECK (status IN ('PENDING_APPROVAL', 'APPROVED', 'POSTED', 'REVERSED'))
);

ALTER TABLE consolidation_adjustments ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consolidation_adjustments FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_consolidation_adjustments_tenant_group_period
    ON consolidation_adjustments (tenant_id, group_legal_entity_id, fiscal_period);
