-- 000019 down: remove the seeded §10.1 baseline.
DELETE FROM sod_rules WHERE sod_rule_id::text LIKE '00000000-0000-0000-0101-%';
