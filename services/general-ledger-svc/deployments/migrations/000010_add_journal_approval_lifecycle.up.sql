-- Migration: 000010_add_journal_approval_lifecycle.up.sql
--
-- ACC-03 (Journal Entry): "owns Journal proposal/lifecycle. Must never
-- own: Append-only ledger truth." Fuller ownership: "JournalHeader,
-- JournalLine proposal, journal lifecycle state, approval subject
-- fingerprint and posting reference." State model (verbatim): "Draft →
-- PendingApproval → Approved → PostingRequested → Posted;
-- rejected/cancelled before posting; corrections create new journals."
--
-- This is a SEPARATE lifecycle from journal_headers.status (PENDING ->
-- VALIDATED -> FINALIZED -> REVERSED), which is ACC-04/05's own Tri-Phase
-- Commit for the actual ledger write. The doc's own invariant states this
-- explicitly: "Journal ≠ ledger entry: ACC-03 owns journal proposal/
-- lifecycle. Posted accounting truth remains in ACC-05." approval_status
-- governs whether a journal is even ELIGIBLE to enter the posting
-- engine — a journal only reaches PostApprovedJournal (ACC-04) once it is
-- POSTING_REQUESTED, and approval_status only advances to POSTED once
-- ACC-04 actually finalizes it. Two lifecycles, composed, neither owning
-- the other's authority.
--
-- approval_fingerprint is the spec's own named evidence field ("approval
-- subject fingerprint"): a permanent hash of exactly what content was
-- approved, captured at the moment of approval. Content can never change
-- after APPROVED in this design (no endpoint edits a journal past DRAFT/
-- PENDING_APPROVAL — see handler.go's AmendDraftJournal), so the
-- fingerprint is pure permanent evidence of what a human actually signed
-- off on, not a live concurrency gate.
--
-- No "configured journal classes" service exists to decide which
-- journals require maker/checker (the spec's own words: "Maker/checker
-- for CONFIGURED journal classes"), so this v1 makes maker/checker
-- universal — every journal requires a different principal to submit and
-- approve it — the same "no config service exists yet, so apply the
-- safer universal default" posture as ACC-01's bootstrap-gap doctrine
-- applied in the opposite direction (there, an absent registry loosens a
-- check; here, an absent registry tightens one, because loosening
-- segregation of duties by default is the wrong side to bootstrap-gap
-- toward).

ALTER TABLE journal_headers
    ADD COLUMN approval_status VARCHAR(20) NOT NULL DEFAULT 'DRAFT', -- DRAFT|PENDING_APPROVAL|APPROVED|POSTING_REQUESTED|POSTED|REJECTED|CANCELLED
    ADD COLUMN approval_fingerprint VARCHAR(64),
    ADD COLUMN submitted_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN submitted_by_principal_id VARCHAR(255),
    ADD COLUMN approved_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN approved_by_principal_id VARCHAR(255),
    ADD COLUMN rejected_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN rejected_by_principal_id VARCHAR(255),
    ADD COLUMN rejection_reason TEXT,
    ADD COLUMN posting_requested_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN posting_requested_by_principal_id VARCHAR(255),
    -- correction_of_journal_id is the spec's own "correction chain"
    -- evidence: "corrections create new journals," never an in-place edit
    -- of a posted one. Self-referencing, nullable — only set on a journal
    -- created via RequestCorrection.
    ADD COLUMN correction_of_journal_id UUID;

-- Every existing row (created before this migration, or via a caller that
-- never goes through the ACC-03 workflow, e.g. ACC-04's own
-- PostAccountingEvent for system-originated postings) is backfilled
-- POSTED if it already reached FINALIZED — a real journal a human never
-- had to approve is not fiction; it is the deliberate ACC-04 system-event
-- path the spec itself carves out. Anything still PENDING/VALIDATED is
-- left DRAFT, the honest default for a proposal nobody has acted on.
UPDATE journal_headers SET approval_status = 'POSTED' WHERE status = 'FINALIZED';
UPDATE journal_headers SET approval_status = 'POSTED' WHERE status = 'REVERSED';

CREATE INDEX idx_journal_headers_approval_status ON journal_headers (tenant_id, legal_entity_id, approval_status);
CREATE INDEX idx_journal_headers_correction_of ON journal_headers (tenant_id, correction_of_journal_id) WHERE correction_of_journal_id IS NOT NULL;
