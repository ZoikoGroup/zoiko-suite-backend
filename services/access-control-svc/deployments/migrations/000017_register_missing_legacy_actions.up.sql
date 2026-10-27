-- Migration 000017: register the legacy action names services authorize
-- against that 000008 missed.
--
-- 000008 grandfathered "every action constant a service authorizes against
-- (6 Oct 2026)". Services added or renamed actions after that snapshot, and a
-- scan on 12 Oct 2026 (each service's Go action constants, against this
-- registry) found 68 that are enforced at /v1/authorize but not registered
-- here. A bundle may only name registered actions, so no role could be
-- authored with any of them through this service, and every one of those
-- checks could only ever answer DENIED — fail closed, but permanently, with no
-- governed way to grant (authorization-svc audit row "action names callers use
-- but nothing grants").
--
-- Same terms as 000008's LEGACY block: STANDARD risk, grandfathered names.
-- The description names the service that enforces each, so the entry can be
-- traced to its caller. Raising any to HIGH (which makes an assignment of it
-- need independent approval) is the IAM policy owner's decision, not this
-- migration's.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('APPROVE_VARIANCE', 'LEGACY', 'STANDARD', 'Enforced by accounts-payable-svc'),
    ('CLAUSE_ACTIVATE', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('CLAUSE_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('CLAUSE_RETIRE', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('CLAUSE_SUBMIT_LEGAL_REVIEW', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('CLAUSE_SUPERSEDE', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('CONTRACT_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by contract-lifecycle-svc'),
    ('CONTRACT_RECORD_EXECUTION', 'LEGACY', 'STANDARD', 'Enforced by contract-lifecycle-svc'),
    ('CONTRACT_RENEW', 'LEGACY', 'STANDARD', 'Enforced by contract-lifecycle-svc'),
    ('CONTRACT_SEND_FOR_SIGNATURE', 'LEGACY', 'STANDARD', 'Enforced by contract-lifecycle-svc'),
    ('CONTRACT_SUBMIT_REVIEW', 'LEGACY', 'STANDARD', 'Enforced by contract-lifecycle-svc'),
    ('CURRENCY_ACTIVATE', 'LEGACY', 'STANDARD', 'Enforced by currency-registry-svc'),
    ('CURRENCY_IMPORT', 'LEGACY', 'STANDARD', 'Enforced by currency-registry-svc'),
    ('CURRENCY_RESTRICT', 'LEGACY', 'STANDARD', 'Enforced by currency-registry-svc'),
    ('CURRENCY_RETIRE', 'LEGACY', 'STANDARD', 'Enforced by currency-registry-svc'),
    ('CURRENCY_TENANT_DISABLE', 'LEGACY', 'STANDARD', 'Enforced by currency-registry-svc'),
    ('CURRENCY_TENANT_ENABLE', 'LEGACY', 'STANDARD', 'Enforced by currency-registry-svc'),
    ('DEVIATION_RULE_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('DEVIATION_RULE_CREATE', 'LEGACY', 'STANDARD', 'Enforced by clause-template-svc'),
    ('EXPORT_PACKAGE_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('EXPORT_PACKAGE_READ', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('FISCAL_CALENDAR_ACTIVATE', 'LEGACY', 'STANDARD', 'Enforced by fiscal-calendar-svc'),
    ('FISCAL_CALENDAR_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by fiscal-calendar-svc'),
    ('FISCAL_CALENDAR_PROPOSE', 'LEGACY', 'STANDARD', 'Enforced by fiscal-calendar-svc'),
    ('FIXITY_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('FIXITY_READ', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('GL_SOFT_CLOSE_POSTING_OVERRIDE', 'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc'),
    ('IDENTITY_SUPPORT_CONTEXT_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by identity-context-svc'),
    ('INVOICE_MATCH_EXCEPTION_RESOLVE', 'LEGACY', 'STANDARD', 'Enforced by accounts-payable-svc'),
    ('INVOICE_MATCH_READ', 'LEGACY', 'STANDARD', 'Enforced by accounts-payable-svc'),
    ('INVOICE_MATCH_RUN', 'LEGACY', 'STANDARD', 'Enforced by accounts-payable-svc'),
    ('MATCH_POLICY_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by accounts-payable-svc'),
    ('OBLIGATION_COMPLETE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_DISPUTE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_MARK_DUE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_MARK_IN_PROGRESS', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_RECORD_BREACH', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_SCHEDULE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_SUPERSEDE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_VALIDATE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('OBLIGATION_WAIVE', 'LEGACY', 'STANDARD', 'Enforced by obligation-tracking-svc'),
    ('PERIOD_CLOSE_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by financial-close-svc'),
    ('PERIOD_CLOSE_EXCLUSION_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by financial-close-svc'),
    ('PERIOD_MATERIALIZE', 'LEGACY', 'STANDARD', 'Enforced by accounting-period-svc'),
    ('PERIOD_REOPEN_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by financial-close-svc'),
    ('PERIOD_STATE_COMMAND', 'LEGACY', 'STANDARD', 'Enforced by accounting-period-svc'),
    ('PO_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_APPROVE_HIGHVALUE', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_CANCEL', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_CREATE', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_HOLD', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_PROGRESS_RECORD', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_SUBMIT', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PO_SUPPLIER_EXCEPTION', 'LEGACY', 'STANDARD', 'Enforced by purchase-order-svc'),
    ('PROJECT_MILESTONE_APPROVE', 'LEGACY', 'STANDARD', 'Enforced by project-accounting-svc'),
    ('PROJECT_MILESTONE_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by project-accounting-svc'),
    ('PR_REQUEST_CANCEL', 'LEGACY', 'STANDARD', 'Enforced by purchase-request-svc'),
    ('PR_REQUEST_CONVERT', 'LEGACY', 'STANDARD', 'Enforced by purchase-request-svc'),
    ('PR_REQUEST_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by purchase-request-svc'),
    ('PR_REQUEST_READ', 'LEGACY', 'STANDARD', 'Enforced by purchase-request-svc'),
    ('PR_REQUEST_SUBMIT', 'LEGACY', 'STANDARD', 'Enforced by purchase-request-svc'),
    ('REDACTION_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('REDACTION_READ', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('RENDITION_MANAGE', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('RENDITION_READ', 'LEGACY', 'STANDARD', 'Enforced by document-vault-svc'),
    ('RESOLUTION_CLOSE_VOTING', 'LEGACY', 'STANDARD', 'Enforced by board-resolutions-svc'),
    ('RESOLUTION_OPEN_VOTING', 'LEGACY', 'STANDARD', 'Enforced by board-resolutions-svc'),
    ('RESOLUTION_SUPERSEDE', 'LEGACY', 'STANDARD', 'Enforced by board-resolutions-svc')
ON CONFLICT (action_name) DO NOTHING;
