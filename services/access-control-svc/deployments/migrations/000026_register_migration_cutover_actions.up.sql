-- Migration 000023: register the actions the migration and cutover commands of the Global Accounting Kernel check (standard §28, §30),
-- and give them role templates. Staging, approving and posting an opening balance are three different people: the migration authority
-- that approves is named, time-bound and never the person who staged the batch.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('GL_MIGRATION_STAGE',     'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: stage, validate or cancel a migration batch of opening balances'),
    ('GL_MIGRATION_READ',      'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read migration batches, their lines, preview and evidence'),
    ('GL_MIGRATION_APPROVE',   'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: approve a migration batch as the named migration authority and set its cutover window (never the person who staged it)'),
    ('GL_MIGRATION_POST',      'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: post an approved migration batch as the opening journal, inside its cutover window'),
    ('GL_MIGRATION_RECONCILE', 'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: reconcile a posted migration batch to the source and subledger totals and freeze its evidence')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('GL_MIGRATION_PREPARER',   'Migration Preparer',   'Stage and validate opening-balance batches from a source extract and cancel them; cannot approve or post.', 'LEGAL_ENTITY', FALSE),
    ('GL_MIGRATION_AUTHORITY',  'Migration Authority',  'Approve migration batches and set the cutover window; holds no staging or posting action.', 'LEGAL_ENTITY', FALSE),
    ('GL_MIGRATION_POSTER',     'Migration Poster',     'Post approved migration batches inside their window and reconcile them; cannot stage or approve.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('GL_MIGRATION_PREPARER',  1, ARRAY['GL_MIGRATION_STAGE','GL_MIGRATION_READ'], 'Initial template (Global Accounting Kernel, section 28)', 'migration:000023'),
    ('GL_MIGRATION_AUTHORITY', 1, ARRAY['GL_MIGRATION_APPROVE','GL_MIGRATION_READ'], 'Initial template (Global Accounting Kernel, section 28)', 'migration:000023'),
    ('GL_MIGRATION_POSTER',    1, ARRAY['GL_MIGRATION_POST','GL_MIGRATION_RECONCILE','GL_MIGRATION_READ'], 'Initial template (Global Accounting Kernel, section 28)', 'migration:000023'),
    -- the read-only archetype reads migration evidence too: a new version, never an edit
    ('VIEWER_AUDITOR_READ_ONLY', 4, ARRAY['audit_evidence.read','audit_evidence.list','audit_evidence.export',
                                          'WORKFLOW_READ','OBLIGATION_READ','OBLIGATION_DEFINITION_READ','FILING_REQUIREMENT_READ',
                                          'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ','GOVERNANCE_DECISION_READ','POLICY_READ',
                                          'GL_BOOK_READ','GL_EVENT_READ','GL_RULE_READ','GL_DIMENSION_READ','GL_RECON_READ','GL_PERIOD_READ',
                                          'GL_FX_RATE_READ','GL_REVALUATION_READ','GL_SNAPSHOT_READ','GL_CLOSE_EVIDENCE_READ',
                                          'GL_CONSOLIDATION_READ','GL_ALLOCATION_READ','GL_POSTING_VIEW','GL_MIGRATION_READ'],
                                          'Adds the migration evidence reads', 'migration:000023')
ON CONFLICT (template_code, template_version) DO NOTHING;

DO $$
DECLARE missing TEXT;
BEGIN
    SELECT string_agg(DISTINCT a, ', ') INTO missing
      FROM role_template_versions v, unnest(v.permitted_actions) a
     WHERE NOT EXISTS (SELECT 1 FROM permission_definitions p WHERE p.action_name = a AND p.active_flag);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'role template actions not in permission_definitions: %', missing;
    END IF;
END
$$;
