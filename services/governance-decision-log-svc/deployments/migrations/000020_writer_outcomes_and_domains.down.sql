-- Migration: 000020_writer_outcomes_and_domains.down.sql
DELETE FROM decision_outcome_categories
WHERE outcome IN ('REQUIRE_APPROVAL', 'CERTIFIED', 'RELEASED', 'PUBLISHED', 'ACTIVATED', 'UNBLOCKED',
                  'SUPERSEDED', 'WITHDRAWN', 'ROLLED_BACK', 'RAISED', 'WAIVED', 'SATISFIED', 'MISSING',
                  'NO_REQUIREMENTS_DEFINED', 'APPROVE', 'REJECT', 'CANCELLED', 'INVALIDATED', 'EXPIRED',
                  'REOPENED');

DELETE FROM decision_domain_rules
WHERE keyword IN ('JURISDICTION', 'RULE_PACK', 'INTERPRETATION', 'REGULATORY', 'CALENDAR', 'CLOCK',
                  'WAIVE', 'AUTHORIZ', 'IAM.', 'BREAK_GLASS', 'SESSION');
