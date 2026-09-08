-- Migration: 000003_add_asset_event.up.sql
--
-- AST-03 (Asset Event): "owns AssetEvent. Must never own: General Ledger
-- or unsupported valuation/legal conclusions." Fuller ownership (verbatim):
-- "AssetEvent; event type; affected asset/component/book; source basis;
-- value/effective date; approval fingerprint; resulting book-state delta;
-- correction/supersession links." Purpose (verbatim): "Record governed
-- economic lifecycle events that change asset book state, including
-- capitalization, additions, component replacement, transfer,
-- impairment/revaluation and disposal, while preserving immutable
-- history."
--
-- State model (verbatim): "Draft→Validated→Approved→Applied→
-- AccountingEventEmitted→Reconciled; Rejected/Cancelled before apply;
-- applied events corrected only by reverse/supersede."
--
-- Explicit commands (verbatim): "CreateAssetEvent; ValidateAssetEvent;
-- ApproveAssetEvent; ApplyAssetEvent; ReverseAssetEvent;
-- SupersedeAssetEvent; RecordDisposal; RecordImpairment; RecordRevaluation;
-- RecordComponentReplacement." The same doc-vs-command mismatch pattern
-- already found across ACC-08/09/10/17 and AST-01/02 in this platform
-- recurs here, twice over:
--   * No command named RejectAssetEvent or CancelAssetEvent exists,
--     despite "Rejected/Cancelled before apply" being named in the state
--     model — left unreachable in this v1, stated honestly, the same
--     posture as AST-02's own Reconciled/Certified gap. A caller who
--     changes their mind about a DRAFT/VALIDATED/APPROVED event simply
--     never calls ApplyAssetEvent against it.
--   * No command named EmitAssetEventAccountingEvent exists either, even
--     though "AccountingEventEmitted" is its own named state — ApplyAssetEvent
--     performs the book-state delta AND (where the event carries a $ amount)
--     the ledger posting in one step, landing the row directly in
--     ACCOUNTING_EVENT_EMITTED — the same collapse ValidateDepreciationRun
--     already applies to Run's own Calculated state (migration 000002).
--     An event with no $ impact (e.g. a pure TRANSFER or
--     COMPONENT_REPLACEMENT record) lands in APPLIED and stays there —
--     never a bug, since it never had anything to post.
--   * RecordDisposal/RecordImpairment/RecordRevaluation/
--     RecordComponentReplacement are built as thin, type-specific entry
--     points into the one real create path (CreateAssetEvent), each
--     pre-setting event_type and enforcing that type's own required
--     evidence — not four independent state machines.
--
-- Scope narrowing, stated honestly up front: of the seven event types
-- ("capitalization, additions, component replacement, transfer,
-- impairment/revaluation and disposal"), only DISPOSAL drives a real,
-- unambiguous book-state delta in this v1 — ApplyAssetEvent transitions
-- fixed_assets.status ACTIVE/SUSPENDED → DISPOSED, the one new status
-- AST-01's own migration 000001 explicitly reserved for AST-03 ("Disposed
-- ... driven by accepted events"). The other six event types are recorded
-- as permanent, evidenced, immutable history (this table's whole reason
-- to exist) but do not themselves rewrite AST-01/AST-02 state in this v1
-- — e.g. an applied IMPAIRMENT event does not yet reduce AST-02's own
-- schedule cost_basis, and CAPITALIZATION/ADDITION events do not
-- themselves drive AST-01's RequestCapitalization. Wiring each of those
-- six into their own downstream mutation is real, non-trivial follow-on
-- work per event type, deliberately deferred — the same "deliberately
-- out of scope, stated honestly" posture used throughout this session
-- (e.g. AST-02's own Run Reconciled/Certified gap).
--
-- Minimum negative-path acceptance (verbatim, all four):
--   1. "Impairment amount entered without evidence/approval" — ValidateAssetEvent
--      refuses (application-level, ErrValuationEvidenceRequired) to move an
--      IMPAIRMENT or REVALUATION event out of DRAFT without a recorded
--      valuation_evidence_ref. Approval is separately enforced by the SoD
--      rule below.
--   2. "Disposed asset event edited in place" — enforced structurally by
--      trg_reject_asset_event_economic_mutation below: once an event's
--      status is APPLIED or later, its own economic facts (event_type,
--      asset_id, amount, proceeds_amount, valuation_evidence_ref,
--      source_document_ref, effective_date, currency) can never be
--      UPDATEd again, regardless of application-code bugs — a real,
--      DB-enforced guard, not merely a documented convention. The only
--      way to correct an applied event is ReverseAssetEvent/
--      SupersedeAssetEvent, which touch status/reversal metadata only.
--   3. "Cross-entity asset transfer bypasses intercompany accounting" —
--      chk_asset_event_no_cross_entity_transfer below refuses, at the
--      database layer, any TRANSFER event whose destination_legal_entity_id
--      differs from the event's own legal_entity_id — this service does
--      not attempt intercompany treatment itself (BNK/AR/intercompany
--      services are a documented dependency, not built against here); it
--      simply blocks the unsafe path outright, matching the spec's own
--      failure semantics: "Direct cross-legal-entity transfer is blocked:
--      ownership change requires intercompany/disposal-acquisition
--      treatment."
--   4. "Hard-closed-period event silently backdated" — ApplyAssetEvent
--      calls financial-close-svc's real period-status endpoint
--      (internal/clients.Clients.CheckPeriodOpen, added alongside this
--      migration) before applying; a LOCKED/CLOSED period refuses
--      (ErrPeriodLocked), and an unreachable financial-close-svc fails
--      CLOSED (ErrPeriodCheckUnavailable) — mirrors general-ledger-svc's
--      own internal/close.Client exactly.
--
-- Segregation of duties (verbatim): "Event initiator cannot approve
-- material impairment/revaluation/disposal where maker-checker applies."
-- No finer-grained materiality concept exists in this platform (the same
-- gap every other capability in this session has hit), so self-approval is
-- refused for exactly the three event types the spec itself names as
-- material — IMPAIRMENT, REVALUATION, DISPOSAL — and permitted for the
-- other four, which carry no approval-time $ consequence in this v1.
--
-- asset_events is a normal mutable stateful row walking the state model
-- above (like depreciation_runs, not like the append-only
-- depreciation_lines) — mutability is required for status/approval/
-- reversal metadata to ever be recorded at all. What must NEVER mutate
-- once applied is the event's own economic substance, enforced by the
-- reject-trigger described in negative path #2 above.
CREATE TABLE asset_events (
    event_id                     UUID PRIMARY KEY,
    tenant_id                      VARCHAR(255) NOT NULL,
    legal_entity_id                  VARCHAR(255) NOT NULL,
    asset_id                           UUID NOT NULL REFERENCES fixed_assets(asset_id),
    component_id                        UUID REFERENCES asset_components(component_id), -- COMPONENT_REPLACEMENT only
    event_type                            VARCHAR(30) NOT NULL,
    status                                   VARCHAR(30) NOT NULL,
    source_document_ref                       VARCHAR(255) NOT NULL,
    valuation_evidence_ref                      VARCHAR(255), -- required by ValidateAssetEvent for IMPAIRMENT/REVALUATION
    amount                                        NUMERIC(18,2),
    currency                                        VARCHAR(3),
    effective_date                                    DATE NOT NULL,
    fiscal_period                                       VARCHAR(20) NOT NULL, -- the period ApplyAssetEvent checks against financial-close-svc
    proceeds_amount                                       NUMERIC(18,2), -- DISPOSAL only; "proceeds where applicable"
    destination_custodian_id                                VARCHAR(255),
    destination_location_id                                   VARCHAR(255),
    destination_legal_entity_id                                 VARCHAR(255), -- TRANSFER only; see chk_asset_event_no_cross_entity_transfer
    debit_account_code                                            VARCHAR(64), -- caller-supplied, same bootstrap posture as AST-02's own run account codes
    credit_account_code                                             VARCHAR(64),
    journal_id                                                        VARCHAR(255),
    correction_of_event_id                                              UUID REFERENCES asset_events(event_id), -- reverse/supersede chain

    created_at                    TIMESTAMP WITH TIME ZONE NOT NULL,
    created_by_principal_id         VARCHAR(255) NOT NULL,
    validated_at                       TIMESTAMP WITH TIME ZONE,
    approved_at                          TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id               VARCHAR(255),
    applied_at                               TIMESTAMP WITH TIME ZONE,
    emitted_at                                 TIMESTAMP WITH TIME ZONE,
    reversed_at                                  TIMESTAMP WITH TIME ZONE,
    reversed_by_principal_id                       VARCHAR(255),
    reversal_reason                                  TEXT,
    superseded_at                                      TIMESTAMP WITH TIME ZONE,
    superseded_by_principal_id                           VARCHAR(255),
    supersession_reason                                    TEXT,

    CONSTRAINT chk_asset_event_type CHECK (event_type IN (
        'CAPITALIZATION', 'ADDITION', 'COMPONENT_REPLACEMENT', 'TRANSFER', 'IMPAIRMENT', 'REVALUATION', 'DISPOSAL'
    )),
    CONSTRAINT chk_asset_event_status CHECK (status IN (
        'DRAFT', 'VALIDATED', 'APPROVED', 'APPLIED', 'ACCOUNTING_EVENT_EMITTED', 'REVERSED', 'SUPERSEDED'
    )),
    -- Negative path #3, "Cross-entity asset transfer bypasses intercompany
    -- accounting" — enforced permanently at insert time, not merely at
    -- apply time, since there is never a legitimate reason for this row to
    -- exist in the first place.
    CONSTRAINT chk_asset_event_no_cross_entity_transfer CHECK (
        event_type != 'TRANSFER' OR destination_legal_entity_id IS NULL OR destination_legal_entity_id = legal_entity_id
    )
);

-- Negative path #2, "Disposed asset event edited in place" — real,
-- DB-enforced immutability of an applied event's own economic substance.
-- Status/approval/reversal metadata columns are deliberately NOT checked
-- here — ReverseAssetEvent/SupersedeAssetEvent must still be able to
-- update status and their own timestamp/reason columns after APPLIED.
CREATE OR REPLACE FUNCTION reject_asset_event_economic_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('APPLIED', 'ACCOUNTING_EVENT_EMITTED', 'REVERSED', 'SUPERSEDED') AND (
        NEW.event_type IS DISTINCT FROM OLD.event_type OR
        NEW.asset_id IS DISTINCT FROM OLD.asset_id OR
        NEW.amount IS DISTINCT FROM OLD.amount OR
        NEW.proceeds_amount IS DISTINCT FROM OLD.proceeds_amount OR
        NEW.valuation_evidence_ref IS DISTINCT FROM OLD.valuation_evidence_ref OR
        NEW.source_document_ref IS DISTINCT FROM OLD.source_document_ref OR
        NEW.effective_date IS DISTINCT FROM OLD.effective_date OR
        NEW.currency IS DISTINCT FROM OLD.currency
    ) THEN
        RAISE EXCEPTION 'asset_events: economic fields of an applied event are immutable — use ReverseAssetEvent/SupersedeAssetEvent, not UPDATE';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_asset_event_economic_mutation
    BEFORE UPDATE ON asset_events
    FOR EACH ROW EXECUTE FUNCTION reject_asset_event_economic_mutation();

ALTER TABLE asset_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON asset_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE INDEX idx_asset_events_entity ON asset_events (tenant_id, legal_entity_id);
CREATE INDEX idx_asset_events_asset ON asset_events (tenant_id, asset_id);

-- Extends AST-01's own fixed_assets.status CHECK to add DISPOSED — the
-- status migration 000001's own doc comment explicitly reserved for
-- AST-03: "Disposed and HeldForSale are explicitly 'driven by accepted
-- events' — AST-03's own authority, not reachable through any AST-01
-- command." HeldForSale remains unreached in this v1 (no event type or
-- command in the spec names it as a distinct trigger) — stated honestly,
-- same posture as every other unreachable named state this session.
ALTER TABLE fixed_assets DROP CONSTRAINT chk_fixed_asset_status;
ALTER TABLE fixed_assets ADD CONSTRAINT chk_fixed_asset_status
    CHECK (status IN ('CANDIDATE', 'REGISTERED', 'ACTIVE', 'SUSPENDED', 'MERGED', 'DISPOSED'));
