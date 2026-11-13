-- 000037 down: nothing to undo. The up adds only what 000005 and 000011 define
-- and a fresh database already has; removing it would break those migrations'
-- schema. Their own down files remove these objects.
SELECT 1;
