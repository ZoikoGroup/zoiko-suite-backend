-- Migration 000059: register the comment moderation action (BIZ-09), enforced by comments-collaboration-svc.
-- Moderating a comment, and deleting another principal's comment, needs COMMENT_MODERATE, apart from the
-- COMMENT_MANAGE that ordinary participation uses (the spec's moderation separate from business authority).
-- Moderation keeps the original text in the append-only history and records a reason, so STANDARD. Anyone who
-- relied on COMMENT_MANAGE alone for moderation must now be granted this. Who holds it is a role-design
-- decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('COMMENT_MODERATE', 'LEGACY', 'STANDARD', 'Enforced by comments-collaboration-svc: moderate a comment, or delete another principal''s comment (never one''s own for moderation)')
ON CONFLICT (action_name) DO NOTHING;
