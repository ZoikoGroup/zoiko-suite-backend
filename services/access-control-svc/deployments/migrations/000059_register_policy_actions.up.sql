-- Migration 000059: register the remaining core policy-svc actions for policy families
-- that were not covered by 000008 (permission_taxonomy) or 000019 (GOV-05) or 000057/000058.
-- These are enforced by policy-svc (D2-3).
-- POLICY_CREATE: STANDARD (tenant-scoped family creation) - update description from 000008
-- Note: POLICY_VERSION_CREATE and POLICY_VERSION_CREATE_GLOBAL already registered in 000008

-- Update existing POLICY_CREATE description from 000008 (was empty)
UPDATE permission_definitions
SET description = 'Enforced by policy-svc: create a policy family'
WHERE action_name = 'POLICY_CREATE' AND description = '';