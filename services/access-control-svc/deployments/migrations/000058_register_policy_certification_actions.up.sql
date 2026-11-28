-- Migration 000058: register the V-001 PDC-05 certification actions, enforced by policy-svc.
-- A family's test suite, running it, waiving a failed case and simulating a candidate are separate capabilities
-- (V-001 §25). Maintaining the suite and running it decide whether a version may go live, so they are HIGH. A
-- waiver excuses a failed mandatory case: requesting one is STANDARD, approving one is HIGH, and policy-svc refuses an
-- approval by the requester whoever holds the permission. Simulation reads the tenant's recorded decisions and writes
-- only its own results: STANDARD. Who holds these is a role-design decision, so no role template is created.

INSERT INTO permission_definitions (action_name, naming, risk_tier, description) VALUES
    ('POLICY_TEST_MANAGE',    'LEGACY', 'HIGH',     'Enforced by policy-svc: add or retire a test case of a policy family''s certification suite'),
    ('POLICY_TEST_RUN',       'LEGACY', 'HIGH',     'Enforced by policy-svc: run a family''s certification suite against a policy version'),
    ('POLICY_WAIVER_REQUEST', 'LEGACY', 'STANDARD', 'Enforced by policy-svc: request a time-bounded waiver of a failed mandatory test case'),
    ('POLICY_WAIVER_APPROVE', 'LEGACY', 'HIGH',     'Enforced by policy-svc: approve or revoke a test waiver; never by its requester'),
    ('POLICY_SIMULATE',       'LEGACY', 'STANDARD', 'Enforced by policy-svc: simulate a candidate policy version over the tenant''s recorded decisions')
ON CONFLICT (action_name) DO NOTHING;
