-- Migration 000059 down: restore the empty description 000008 registered
-- POLICY_CREATE with. The action itself belongs to 000008 and stays.
UPDATE permission_definitions
SET description = ''
WHERE action_name = 'POLICY_CREATE' AND description = 'Enforced by policy-svc: create a policy family';
