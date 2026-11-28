-- Migration 000032: a case records the source that opened it, and one source
-- milestone opens one case.
--
-- R-001 §6.3: a case is "created with type, source, tenant, scope and initial
-- severity". obligations-svc's escalation relay opens a case per clock
-- milestone (WF-S7, OB-S11) with source_type / source_id / milestone and
-- retries until it gets an answer, so a retry after a lost response must
-- return the case it already opened, never a second one. Until 26 Nov 2026
-- the body was refused 400 (no case_type, no workflow_instance_id) and the
-- source was not stored anywhere.

ALTER TABLE cases
    ADD COLUMN source_type TEXT,
    ADD COLUMN source_id   TEXT,
    ADD COLUMN milestone   TEXT;

ALTER TABLE cases
    ADD CONSTRAINT cases_source_complete CHECK (
        (source_type IS NULL AND source_id IS NULL AND milestone IS NULL)
        OR (source_type IS NOT NULL AND source_id IS NOT NULL)
    );

CREATE UNIQUE INDEX cases_source_milestone_unique
    ON cases (tenant_id, source_type, source_id, COALESCE(milestone, ''))
    WHERE source_type IS NOT NULL;
