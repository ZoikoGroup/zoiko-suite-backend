-- 000010_add_com03_entitlements.down.sql

DROP INDEX IF EXISTS idx_subscriptions_org_window;
DROP TABLE IF EXISTS commercial_restrictions;
DROP TABLE IF EXISTS entitlement_policy_versions;
DROP FUNCTION IF EXISTS enforce_restriction_lift_only();
DROP FUNCTION IF EXISTS reject_immutable_row();
