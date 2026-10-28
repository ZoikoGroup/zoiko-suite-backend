-- Migration 000019: register the CLOSE_REVIEW close-journal action.
--
-- ACC-14's CLOSE_REVIEW posting policy admits only "designated close
-- journals/workflows". general-ledger-svc enforces that with a journal-level
-- close_journal_designation plus this separate action. 000008/000017
-- grandfathered the actions that existed then; this one is new, and a bundle
-- may only name a registered action, so without this row no role could ever be
-- granted the action and the CLOSE_REVIEW path would fail closed permanently.
--
-- LEGACY naming and STANDARD risk follow the GL_SOFT_CLOSE_POSTING_OVERRIDE
-- entry in 000017. Raising it to HIGH (which would require independent
-- approval for every assignment) is the IAM policy owner's decision.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('GL_CLOSE_REVIEW_POSTING', 'LEGACY', 'STANDARD',
     'Enforced by general-ledger-svc: post a designated close journal during CLOSE_REVIEW')
ON CONFLICT (action_name) DO NOTHING;
