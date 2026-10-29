-- Migration 000022: register the actions the Global Accounting Kernel (ZS-GAK)
-- in general-ledger-svc checks, and give them role templates.
--
-- A bundle may only name registered actions, so until these exist every kernel
-- gate can only answer DENIED to anyone but a deployment that seeded them by
-- hand. 000008 registered the legacy GL_JOURNAL_* / GL_POSTING_* names; the
-- kernel waves added 38 more (books, events, posting rules, dimensions,
-- reconciliation, period seal and FX, revaluation, signed snapshots,
-- consolidation, allocation, recurring journals).
--
-- Risk tiers: anything that changes what a rule posts, seals or reopens a
-- period, signs a balance, certifies a group, or accepts a reconciliation
-- difference is HIGH (an assignment needs independent approval). Running a
-- computation or reading is STANDARD.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('GL_BOOK_MANAGE',            'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: create or change an accounting book (the ledger a rule posts into)'),
    ('GL_BOOK_READ',              'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read accounting books'),
    ('GL_EVENT_REGISTER',         'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: register an immutable accounting event (subledger services and their principals)'),
    ('GL_EVENT_READ',             'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read accounting events'),
    ('GL_RULE_MANAGE',            'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: draft and test a posting rule set or instantiate a standard rule pack'),
    ('GL_RULE_APPROVE',           'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: approve a posting rule set (never the author)'),
    ('GL_RULE_ACTIVATE',          'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: activate or retire an approved posting rule set (never the author)'),
    ('GL_RULE_READ',              'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read posting rule sets and rule packs'),
    ('GL_POSTING_EVALUATE',       'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: dry-run a posting evaluation; writes nothing'),
    ('GL_DIMENSION_MANAGE',       'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: maintain the dimension registry and its values'),
    ('GL_DIMENSION_READ',         'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read the dimension registry'),
    ('GL_JOURNAL_WITHDRAW',       'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: withdraw a draft manual journal with a reason'),
    ('GL_RECURRING_MANAGE',       'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: maintain recurring journal templates'),
    ('GL_RECURRING_GENERATE',     'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: generate draft journals from recurring templates'),
    ('GL_RECON_RUN',              'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: run a subledger-to-GL or event-to-posting reconciliation'),
    ('GL_RECON_READ',             'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read reconciliations, cases and the reconciliation status'),
    ('GL_RECON_RESOLVE',          'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: resolve a reconciliation case with evidence'),
    ('GL_RECON_APPROVE',          'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: accept a reconciliation difference as an exception (never the runner)'),
    ('GL_RECON_TOLERANCE_MANAGE', 'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: version the tolerance a reconciliation is judged against'),
    ('GL_PERIOD_SEAL',            'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: seal a period so nothing posts into it after its watermark'),
    ('GL_PERIOD_REOPEN',          'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: lift a period seal (reopen or reseal)'),
    ('GL_PERIOD_READ',            'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read period seals and their history'),
    ('GL_FX_RATE_MANAGE',         'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: load FX rates and propose a hand-keyed rate override'),
    ('GL_FX_RATE_APPROVE',        'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: approve an FX rate override (second approver)'),
    ('GL_FX_RATE_READ',           'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read FX rates and rate resolutions'),
    ('GL_REVALUATION_RUN',        'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: run unrealised FX revaluation for a period (posts journals)'),
    ('GL_REVALUATION_READ',       'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read revaluation runs and foreign positions'),
    ('GL_SNAPSHOT_SIGN',          'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: sign a period-close or consolidation balance snapshot'),
    ('GL_SNAPSHOT_READ',          'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read and verify signed balance snapshots'),
    ('GL_CLOSE_EVIDENCE_READ',    'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read the kernel close evidence for a period'),
    ('GL_CONSOLIDATION_MANAGE',   'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: maintain consolidation groups, members, mappings and group adjustments'),
    ('GL_CONSOLIDATION_RUN',      'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: run a consolidation (posts into the group book)'),
    ('GL_CONSOLIDATION_CERTIFY',  'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: certify or supersede a consolidation run, approve a group adjustment (never the preparer)'),
    ('GL_CONSOLIDATION_READ',     'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read consolidation groups, runs and intercompany pairings'),
    ('GL_ALLOCATION_MANAGE',      'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: draft or retire an allocation rule'),
    ('GL_ALLOCATION_APPROVE',     'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: approve an allocation rule (never the author)'),
    ('GL_ALLOCATION_RUN',         'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: run or reverse an allocation for a period (posts journals)'),
    ('GL_ALLOCATION_READ',        'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read allocation rules and runs')
ON CONFLICT (action_name) DO NOTHING;

INSERT INTO role_templates (template_code, template_name, default_intent, role_scope_type, is_archetype) VALUES
    ('GL_KERNEL_ADMINISTRATOR',      'Accounting Kernel Administrator',  'Maintain accounting books, dimensions and FX rate loads; changes no posting rule and approves nothing.', 'TENANT', FALSE),
    ('GL_RULE_AUTHOR',               'Posting Rule Author',              'Draft, test and dry-run posting rules and instantiate standard packs; cannot approve or activate what the holder wrote.', 'LEGAL_ENTITY', FALSE),
    ('GL_RULE_APPROVER',             'Posting Rule Approver',            'Approve and activate posting rule sets others wrote (maker-checker with the author).', 'LEGAL_ENTITY', FALSE),
    ('GL_SUBLEDGER_POSTER',          'Subledger Posting Principal',      'Register accounting events and execute postings on behalf of a subledger service (AR, AP, payroll, assets, inventory, tax); granted to the service principal, not a person.', 'LEGAL_ENTITY', FALSE),
    ('GL_RECONCILIATION_ANALYST',    'Reconciliation Analyst',           'Run reconciliations and resolve cases with evidence; cannot accept a difference or change a tolerance.', 'LEGAL_ENTITY', FALSE),
    ('GL_RECONCILIATION_APPROVER',   'Reconciliation Approver',          'Accept reconciliation differences as exceptions and version tolerances (maker-checker with the runner).', 'LEGAL_ENTITY', FALSE),
    ('GL_PERIOD_CLOSE_OPERATOR',     'Kernel Period Close Operator',     'Seal a period, run revaluation, sign close snapshots and read the close evidence; cannot reopen.', 'LEGAL_ENTITY', FALSE),
    ('GL_PERIOD_REOPEN_APPROVER',    'Kernel Period Reopen Approver',    'Lift a period seal; holds no close or posting action.', 'LEGAL_ENTITY', FALSE),
    ('GL_FX_RATE_APPROVER',          'FX Rate Override Approver',        'Approve hand-keyed FX rate overrides proposed by someone else.', 'TENANT', FALSE),
    ('GL_CONSOLIDATION_PREPARER',    'Consolidation Preparer',           'Maintain groups, members and mappings, propose group adjustments and run consolidations; cannot certify.', 'TENANT', FALSE),
    ('GL_CONSOLIDATION_CERTIFIER',   'Consolidation Certifier',          'Certify or supersede consolidation runs and approve group adjustments others proposed.', 'TENANT', FALSE),
    ('GL_ALLOCATION_PREPARER',       'Allocation Preparer',              'Draft allocation rules and run allocations; cannot approve a rule.', 'LEGAL_ENTITY', FALSE),
    ('GL_ALLOCATION_APPROVER',       'Allocation Approver',              'Approve allocation rules others drafted.', 'LEGAL_ENTITY', FALSE),
    ('GL_RECURRING_JOURNAL_OPERATOR','Recurring Journal Operator',       'Maintain recurring journal templates, generate drafts and withdraw drafts; posts nothing.', 'LEGAL_ENTITY', FALSE)
ON CONFLICT (template_code) DO NOTHING;

INSERT INTO role_template_versions (template_code, template_version, permitted_actions, change_note, published_by) VALUES
    ('GL_KERNEL_ADMINISTRATOR',      1, ARRAY['GL_BOOK_MANAGE','GL_BOOK_READ','GL_DIMENSION_MANAGE','GL_DIMENSION_READ','GL_FX_RATE_MANAGE','GL_FX_RATE_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_RULE_AUTHOR',               1, ARRAY['GL_RULE_MANAGE','GL_RULE_READ','GL_POSTING_EVALUATE','GL_BOOK_READ','GL_EVENT_READ','GL_DIMENSION_READ','GL_POSTING_VIEW'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_RULE_APPROVER',             1, ARRAY['GL_RULE_APPROVE','GL_RULE_ACTIVATE','GL_RULE_READ','GL_BOOK_READ','GL_POSTING_VIEW'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_SUBLEDGER_POSTER',          1, ARRAY['GL_EVENT_REGISTER','GL_POSTING_EXECUTE','GL_POSTING_VIEW'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_RECONCILIATION_ANALYST',    1, ARRAY['GL_RECON_RUN','GL_RECON_READ','GL_RECON_RESOLVE','GL_LEDGER_QUERY'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_RECONCILIATION_APPROVER',   1, ARRAY['GL_RECON_APPROVE','GL_RECON_TOLERANCE_MANAGE','GL_RECON_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_PERIOD_CLOSE_OPERATOR',     1, ARRAY['GL_PERIOD_SEAL','GL_PERIOD_READ','GL_REVALUATION_RUN','GL_REVALUATION_READ','GL_SNAPSHOT_SIGN',
                                              'GL_SNAPSHOT_READ','GL_CLOSE_EVIDENCE_READ','GL_FX_RATE_READ','GL_RECON_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_PERIOD_REOPEN_APPROVER',    1, ARRAY['GL_PERIOD_REOPEN','GL_PERIOD_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_FX_RATE_APPROVER',          1, ARRAY['GL_FX_RATE_APPROVE','GL_FX_RATE_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_CONSOLIDATION_PREPARER',    1, ARRAY['GL_CONSOLIDATION_MANAGE','GL_CONSOLIDATION_RUN','GL_CONSOLIDATION_READ','GL_SNAPSHOT_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_CONSOLIDATION_CERTIFIER',   1, ARRAY['GL_CONSOLIDATION_CERTIFY','GL_CONSOLIDATION_READ','GL_SNAPSHOT_SIGN','GL_SNAPSHOT_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_ALLOCATION_PREPARER',       1, ARRAY['GL_ALLOCATION_MANAGE','GL_ALLOCATION_RUN','GL_ALLOCATION_READ','GL_DIMENSION_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_ALLOCATION_APPROVER',       1, ARRAY['GL_ALLOCATION_APPROVE','GL_ALLOCATION_READ'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    ('GL_RECURRING_JOURNAL_OPERATOR',1, ARRAY['GL_RECURRING_MANAGE','GL_RECURRING_GENERATE','GL_JOURNAL_WITHDRAW','GL_JOURNAL_CREATE'],
                                        'Initial template (Global Accounting Kernel)', 'migration:000022'),
    -- The read-only archetype reads the kernel too. A new version, never an
    -- edit: roles on version 2 move only by an explicit upgrade.
    ('VIEWER_AUDITOR_READ_ONLY',     3, ARRAY['audit_evidence.read','audit_evidence.list','audit_evidence.export',
                                              'WORKFLOW_READ','OBLIGATION_READ','OBLIGATION_DEFINITION_READ','FILING_REQUIREMENT_READ',
                                              'EVIDENCE_REQUIREMENT_READ','EVIDENCE_EVALUATION_READ','GOVERNANCE_DECISION_READ','POLICY_READ',
                                              'GL_BOOK_READ','GL_EVENT_READ','GL_RULE_READ','GL_DIMENSION_READ','GL_RECON_READ','GL_PERIOD_READ',
                                              'GL_FX_RATE_READ','GL_REVALUATION_READ','GL_SNAPSHOT_READ','GL_CLOSE_EVIDENCE_READ',
                                              'GL_CONSOLIDATION_READ','GL_ALLOCATION_READ','GL_POSTING_VIEW'],
                                        'Adds the accounting-kernel reads', 'migration:000022')
ON CONFLICT (template_code, template_version) DO NOTHING;

-- Same guard as 000009 / 000021: every template action is a registered, active
-- permission, so a typo fails the migration instead of shipping a role that
-- grants nothing.
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
