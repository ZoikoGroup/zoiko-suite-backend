-- Migration 000018 down. The permission definitions stay: permission_definitions
-- is append-only by design (see 000013's down), and a role granted any of
-- these while this migration was applied should not silently lose the
-- ability to explain what that grant was for.
SELECT 1;
