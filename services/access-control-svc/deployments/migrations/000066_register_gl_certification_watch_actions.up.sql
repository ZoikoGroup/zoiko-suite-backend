-- Migration 000066: register the actions general-ledger-svc enforces for the certification watch (ZS-SVC-Z-001 RB-05, DQC-017): what
-- the ledger does when a certification it relied on is withdrawn. Sweeping (checking watched certifications and flagging what rests
-- on a revoked one) is routine; acknowledging a flag is a person's recorded decision about posted financial data, so it is HIGH.
-- Who holds them is a role-design decision, so no role template is created here.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('GL_CERTIFICATION_WATCH_SWEEP',      'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: sweep the certifications the ledger relies on and flag what rests on one that was revoked'),
    ('GL_CERTIFICATION_FLAG_VIEW',        'LEGACY', 'STANDARD', 'Enforced by general-ledger-svc: read the flags raised on journals and postings resting on a withdrawn certification'),
    ('GL_CERTIFICATION_FLAG_ACKNOWLEDGE', 'LEGACY', 'HIGH',     'Enforced by general-ledger-svc: acknowledge a flag on a posted journal or posting that rests on a withdrawn certification, recording the decision taken')
ON CONFLICT (action_name) DO NOTHING;
