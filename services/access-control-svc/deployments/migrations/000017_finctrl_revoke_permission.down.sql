-- Migration 000017 down. The permission definition stays: permission_definitions
-- is append-only by design (see 000013's down), and a role that was granted
-- FINCTRL_REVOKE while this migration was applied should not silently lose
-- the ability to explain what that grant was for.
SELECT 1;
