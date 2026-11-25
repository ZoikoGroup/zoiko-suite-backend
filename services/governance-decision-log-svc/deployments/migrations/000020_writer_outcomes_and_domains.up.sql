-- Migration: 000020_writer_outcomes_and_domains.up.sql
--
-- Automatic decision capture (tracker "Assignment 25 Nov 2026", step DL-0;
-- Doc 01 s7 Decision Logging Layer). Four more governance engines now relay
-- their verdicts to this ledger: authorization-svc, jurisdiction-rules-svc,
-- obligations-svc and evidence-requirements-svc (writer catalogue:
-- CONTEXT.md "Writers"). Their outcome words and action types are added here,
-- so no relayed decision reads as OTHER in GET /v1/decisions/summary.
--
-- Outcome categories follow 000018's rule: a non-permissive outcome never
-- reads as APPROVED. A withdrawal, a rollback, a block, missing evidence, a
-- cancelled or invalidated or expired workflow is DENIED; an approval that is
-- still required is ESCALATED; a raised obligation and a reopened workflow are
-- PENDING (work now outstanding).
--
-- The workflow words (APPROVE, REJECT, CANCELLED, INVALIDATED, EXPIRED,
-- REOPENED) are workflow-svc's existing outcomes: seven quorum votes in the
-- live ledger read OTHER on 25 Nov 2026 because APPROVE had no row.

INSERT INTO decision_outcome_categories (outcome, category) VALUES
    -- authorization-svc (D-DL-1: only DENY and REQUIRE_APPROVAL are relayed)
    ('REQUIRE_APPROVAL', 'ESCALATED'),
    -- jurisdiction-rules-svc pack lifecycle, calendar and payment terms
    ('CERTIFIED', 'APPROVED'),
    ('RELEASED', 'APPROVED'),
    ('PUBLISHED', 'APPROVED'),
    ('ACTIVATED', 'APPROVED'),
    ('UNBLOCKED', 'APPROVED'),
    ('SUPERSEDED', 'APPROVED'),
    ('WITHDRAWN', 'DENIED'),
    ('ROLLED_BACK', 'DENIED'),
    -- obligations-svc
    ('RAISED', 'PENDING'),
    ('WAIVED', 'APPROVED'),
    -- evidence-requirements-svc
    ('SATISFIED', 'APPROVED'),
    ('MISSING', 'DENIED'),
    ('NO_REQUIREMENTS_DEFINED', 'APPROVED'),
    -- workflow-svc words that had no category
    ('APPROVE', 'APPROVED'),
    ('REJECT', 'DENIED'),
    ('CANCELLED', 'DENIED'),
    ('INVALIDATED', 'DENIED'),
    ('EXPIRED', 'DENIED'),
    ('REOPENED', 'PENDING')
ON CONFLICT (outcome) DO NOTHING;

-- Action-type keywords for the new writers. The lowest precedence wins, then
-- the longest keyword (000016), so these sit at the governance (80) and access
-- (70) levels and never pull a payroll, privacy or finance action out of its
-- domain. authorization-svc and evidence-requirements-svc also send domain
-- explicitly, because their action type embeds the action they evaluated.
INSERT INTO decision_domain_rules (keyword, domain_code, precedence) VALUES
    ('JURISDICTION',   'GOVERNANCE_COMPLIANCE', 80),
    ('RULE_PACK',      'GOVERNANCE_COMPLIANCE', 80),
    ('INTERPRETATION', 'GOVERNANCE_COMPLIANCE', 80),
    ('REGULATORY',     'GOVERNANCE_COMPLIANCE', 80),
    ('CALENDAR',       'GOVERNANCE_COMPLIANCE', 80),
    ('CLOCK',          'GOVERNANCE_COMPLIANCE', 80),
    ('WAIVE',          'GOVERNANCE_COMPLIANCE', 80),
    ('AUTHORIZ',       'ORGANIZATION_ACCESS',   70),
    ('IAM.',           'ORGANIZATION_ACCESS',   70),
    ('BREAK_GLASS',    'ORGANIZATION_ACCESS',   70),
    ('SESSION',        'ORGANIZATION_ACCESS',   70)
ON CONFLICT (keyword) DO NOTHING;
