-- 000019: the §10.1 static conflict matrix, seeded.
--
-- The Authorization Standard §10.1 calls its eight rows the "minimum baseline".
-- sod_rules has had the engine (CheckSoDConflict, CheckOwnObjectSoD,
-- /v1/sod/validate) since the initial schema and no rows: on the dev database
-- the only rule was an audit fixture, so no baseline conflict could fire for
-- any caller, and a role combining payment preparation with payment release
-- passed every check in the estate.
--
-- The rules are GLOBAL (tenant_id NULL), which is what "minimum baseline"
-- means: a tenant may add rules, it cannot opt out of these. They are written
-- in the §5 taxonomy names that access-control-svc's permission registry
-- (its 000008) and §9.1 archetype templates (its 000009) use.
--
-- §10.1 row -> rules:
--   1 Payment Preparer / Payment Releaser        payment.create|prepare <> payment.release
--   2 Supplier Master Bank Editor / Releaser     supplier.bank_details.edit <> payment.release
--   3 Journal Preparer / Journal Final Approver  journal.create <> journal.approve, journal.create <> journal.post
--   4 Period Close Operator / Reopen Approver    period.close <> period.reopen
--   5 Tax Preparer / Tax Sole Filer              tax_return.create <> tax_return.file
--   6 Access Administrator / Own Grant Approver  SELF-scoped, not an action pair: enforced where the
--                                                subject is known (CreateRoleAssignment refuses self-grant;
--                                                access-control-svc refuses self-approval of a request)
--   7 Workpaper Preparer / Same-Workpaper Reviewer  same OBJECT: OWN_OBJECT_FORBIDDEN on workpaper.approve
--   8 Contract Drafter / Sole Contract Signatory contract.create <> contract.execute
--
-- Fixed ids make the seed re-runnable without a uniqueness constraint the
-- table does not have. Severity is carried in conflict_type's suffix only for
-- the reader: the engine matches on the pair and on OWN_OBJECT_FORBIDDEN, and
-- any other conflict_type is a plain static conflict.

INSERT INTO sod_rules (sod_rule_id, domain_code, action_a, action_b, conflict_type, jurisdiction_id, tenant_id) VALUES
    ('00000000-0000-0000-0101-000000000001', 'TREASURY',  'payment.create',             'payment.release',  'STATIC_CRITICAL', NULL, NULL),
    ('00000000-0000-0000-0101-000000000002', 'TREASURY',  'payment.prepare',            'payment.release',  'STATIC_CRITICAL', NULL, NULL),
    ('00000000-0000-0000-0101-000000000003', 'AP',        'supplier.bank_details.edit', 'payment.release',  'STATIC_CRITICAL', NULL, NULL),
    ('00000000-0000-0000-0101-000000000004', 'GL',        'journal.create',             'journal.approve',  'STATIC_HIGH',     NULL, NULL),
    ('00000000-0000-0000-0101-000000000005', 'GL',        'journal.create',             'journal.post',     'STATIC_HIGH',     NULL, NULL),
    ('00000000-0000-0000-0101-000000000006', 'CLOSE',     'period.close',               'period.reopen',    'STATIC_HIGH',     NULL, NULL),
    ('00000000-0000-0000-0101-000000000007', 'TAX',       'tax_return.create',          'tax_return.file',  'STATIC_HIGH',     NULL, NULL),
    ('00000000-0000-0000-0101-000000000008', 'AUDIT',     'workpaper.approve',          'workpaper.approve','OWN_OBJECT_FORBIDDEN', NULL, NULL),
    ('00000000-0000-0000-0101-000000000009', 'LEGAL',     'contract.create',            'contract.execute', 'STATIC_HIGH',     NULL, NULL)
ON CONFLICT (sod_rule_id) DO NOTHING;
