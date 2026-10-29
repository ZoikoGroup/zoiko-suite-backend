-- Migration 000022 down. Refused while any role was made from one of these
-- templates, or any bundle names one of these actions: a role would be left
-- pointing at a template that no longer exists, or holding an unregistered
-- action.
DO $$
DECLARE
    v_used TEXT;
BEGIN
    SELECT string_agg(DISTINCT template_code, ', ') INTO v_used
      FROM role_definitions
     WHERE template_code IN (SELECT template_code FROM role_template_versions WHERE published_by = 'migration:000022')
       AND NOT (template_code = 'VIEWER_AUDITOR_READ_ONLY' AND template_version < 3);
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000022 down refused: roles were made from: %', v_used;
    END IF;
    SELECT string_agg(DISTINCT a, ', ') INTO v_used
      FROM permission_bundle_defs b, unnest(b.permitted_actions) a
     WHERE a IN (SELECT action_name FROM permission_definitions
                  WHERE description LIKE 'Enforced by general-ledger-svc:%'
                    AND action_name IN ('GL_BOOK_MANAGE','GL_BOOK_READ','GL_EVENT_REGISTER','GL_EVENT_READ','GL_RULE_MANAGE','GL_RULE_APPROVE',
                                        'GL_RULE_ACTIVATE','GL_RULE_READ','GL_POSTING_EVALUATE','GL_DIMENSION_MANAGE','GL_DIMENSION_READ',
                                        'GL_JOURNAL_WITHDRAW','GL_RECURRING_MANAGE','GL_RECURRING_GENERATE','GL_RECON_RUN','GL_RECON_READ',
                                        'GL_RECON_RESOLVE','GL_RECON_APPROVE','GL_RECON_TOLERANCE_MANAGE','GL_PERIOD_SEAL','GL_PERIOD_REOPEN',
                                        'GL_PERIOD_READ','GL_FX_RATE_MANAGE','GL_FX_RATE_APPROVE','GL_FX_RATE_READ','GL_REVALUATION_RUN',
                                        'GL_REVALUATION_READ','GL_SNAPSHOT_SIGN','GL_SNAPSHOT_READ','GL_CLOSE_EVIDENCE_READ',
                                        'GL_CONSOLIDATION_MANAGE','GL_CONSOLIDATION_RUN','GL_CONSOLIDATION_CERTIFY','GL_CONSOLIDATION_READ',
                                        'GL_ALLOCATION_MANAGE','GL_ALLOCATION_APPROVE','GL_ALLOCATION_RUN','GL_ALLOCATION_READ'));
    IF v_used IS NOT NULL THEN
        RAISE EXCEPTION '000022 down refused: still granted by a bundle: %', v_used;
    END IF;
END
$$;

-- role_template_versions is append-only by trigger (000009); a down is the one
-- sanctioned removal.
ALTER TABLE role_template_versions DISABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_template_versions WHERE published_by = 'migration:000022';
ALTER TABLE role_template_versions ENABLE TRIGGER role_template_versions_no_update;
DELETE FROM role_templates
 WHERE template_code IN ('GL_KERNEL_ADMINISTRATOR','GL_RULE_AUTHOR','GL_RULE_APPROVER','GL_SUBLEDGER_POSTER','GL_RECONCILIATION_ANALYST',
                         'GL_RECONCILIATION_APPROVER','GL_PERIOD_CLOSE_OPERATOR','GL_PERIOD_REOPEN_APPROVER','GL_FX_RATE_APPROVER',
                         'GL_CONSOLIDATION_PREPARER','GL_CONSOLIDATION_CERTIFIER','GL_ALLOCATION_PREPARER','GL_ALLOCATION_APPROVER',
                         'GL_RECURRING_JOURNAL_OPERATOR');

DELETE FROM permission_definitions
 WHERE description LIKE 'Enforced by general-ledger-svc:%'
   AND action_name IN ('GL_BOOK_MANAGE','GL_BOOK_READ','GL_EVENT_REGISTER','GL_EVENT_READ','GL_RULE_MANAGE','GL_RULE_APPROVE',
                       'GL_RULE_ACTIVATE','GL_RULE_READ','GL_POSTING_EVALUATE','GL_DIMENSION_MANAGE','GL_DIMENSION_READ',
                       'GL_JOURNAL_WITHDRAW','GL_RECURRING_MANAGE','GL_RECURRING_GENERATE','GL_RECON_RUN','GL_RECON_READ',
                       'GL_RECON_RESOLVE','GL_RECON_APPROVE','GL_RECON_TOLERANCE_MANAGE','GL_PERIOD_SEAL','GL_PERIOD_REOPEN',
                       'GL_PERIOD_READ','GL_FX_RATE_MANAGE','GL_FX_RATE_APPROVE','GL_FX_RATE_READ','GL_REVALUATION_RUN',
                       'GL_REVALUATION_READ','GL_SNAPSHOT_SIGN','GL_SNAPSHOT_READ','GL_CLOSE_EVIDENCE_READ',
                       'GL_CONSOLIDATION_MANAGE','GL_CONSOLIDATION_RUN','GL_CONSOLIDATION_CERTIFY','GL_CONSOLIDATION_READ',
                       'GL_ALLOCATION_MANAGE','GL_ALLOCATION_APPROVE','GL_ALLOCATION_RUN','GL_ALLOCATION_READ');
