-- Migration 000051: register the REF-06 accounting-book actions (ZS-SVC-I-001 s4.16), enforced by
-- tenant-entity-registry-svc. Opening a book is a STANDARD act. Activating one, changing its basis, suspending its
-- posting and retiring it decide what ledgers and reports may post to and on what basis, so those are HIGH.
-- tenant-entity-registry-svc additionally refuses a statutory, tax or regulatory book activated by the principal who
-- created it, and a basis change that would rewrite earlier periods, whoever holds the permission. Who holds these is a
-- role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('ACCOUNTING_BOOK_CREATE',          'LEGACY', 'STANDARD', 'Enforced by tenant-entity-registry-svc: open an accounting book, as a draft, with its first basis'),
    ('ACCOUNTING_BOOK_ACTIVATE',        'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: activate a book; a statutory, tax or regulatory one never by its creator'),
    ('ACCOUNTING_BOOK_AMEND_BASIS',     'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: add the next basis version of a book, prospectively'),
    ('ACCOUNTING_BOOK_SUSPEND_POSTING', 'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: suspend posting to a book'),
    ('ACCOUNTING_BOOK_RETIRE',          'LEGACY', 'HIGH',     'Enforced by tenant-entity-registry-svc: retire a book permanently, keeping its history')
ON CONFLICT (action_name) DO NOTHING;
