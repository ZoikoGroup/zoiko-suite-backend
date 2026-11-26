-- Migration 000031: a risk's coverage is recorded against the workpaper that covers it.
--
-- AUD-02 (Audit & Assurance spec §3.2, AUD-CTRL-005): every assessed risk has
-- a planned response and coverage, and unresolved high-risk coverage blocks
-- completion. risk_assessments.coverage_status (000006) starts PENDING and
-- MarkFieldworkComplete refuses while a HIGH risk is not COVERED, but no
-- command ever set COVERED, so any engagement with a HIGH risk could never
-- finish fieldwork (found by the live test of 26 Nov 2026).
--
-- POST /v1/audit/risks/{risk_id}/coverage now records it: the workpaper of the
-- same engagement whose procedures executed the response, who recorded it and
-- when. A materiality change still sends COVERED back to PENDING_REASSESSMENT
-- (RecordMateriality), and coverage is then recorded again.

ALTER TABLE risk_assessments
    ADD COLUMN covered_by_workpaper_id UUID REFERENCES workpapers(workpaper_id),
    ADD COLUMN covered_by_principal_id TEXT,
    ADD COLUMN covered_at              TIMESTAMPTZ;

-- COVERED always names its evidence.
ALTER TABLE risk_assessments
    ADD CONSTRAINT risk_covered_has_evidence CHECK (
        coverage_status <> 'COVERED'
        OR (covered_by_workpaper_id IS NOT NULL AND covered_by_principal_id IS NOT NULL AND covered_at IS NOT NULL)
    ) NOT VALID;
ALTER TABLE risk_assessments VALIDATE CONSTRAINT risk_covered_has_evidence;
