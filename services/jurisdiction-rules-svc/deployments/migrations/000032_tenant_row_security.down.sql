-- 000032 down: remove the tenant row-level security. The tenant columns are kept
-- when they hold data: dropping them would erase which tenant a submission or a
-- decision belonged to, which cannot be recovered.

DROP POLICY IF EXISTS tenant_isolation ON rule_decision_evidence;
ALTER TABLE rule_decision_evidence NO FORCE ROW LEVEL SECURITY;
ALTER TABLE rule_decision_evidence DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_rule_decision_tenant;

DROP POLICY IF EXISTS tenant_isolation ON regulatory_submission_events;
ALTER TABLE regulatory_submission_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE regulatory_submission_events DISABLE ROW LEVEL SECURITY;
ALTER TABLE regulatory_submission_events DROP CONSTRAINT IF EXISTS ck_submission_event_has_tenant;

DROP POLICY IF EXISTS tenant_isolation ON regulatory_submissions;
ALTER TABLE regulatory_submissions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE regulatory_submissions DISABLE ROW LEVEL SECURITY;
ALTER TABLE regulatory_submissions DROP CONSTRAINT IF EXISTS ck_submission_has_tenant;
DROP INDEX IF EXISTS idx_submission_tenant;

DROP POLICY IF EXISTS tenant_isolation ON entity_jurisdiction_applicability;
ALTER TABLE entity_jurisdiction_applicability NO FORCE ROW LEVEL SECURITY;
ALTER TABLE entity_jurisdiction_applicability DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM rule_decision_evidence WHERE tenant_id IS NOT NULL) THEN
        ALTER TABLE rule_decision_evidence DROP COLUMN IF EXISTS tenant_id;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM regulatory_submission_events WHERE tenant_id IS NOT NULL) THEN
        ALTER TABLE regulatory_submission_events DROP COLUMN IF EXISTS tenant_id;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM regulatory_submissions WHERE tenant_id IS NOT NULL) THEN
        ALTER TABLE regulatory_submissions DROP COLUMN IF EXISTS tenant_id;
    END IF;
END
$$;
