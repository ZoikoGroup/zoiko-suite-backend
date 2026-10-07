-- Migration 000005 down: Protected permissions table
DROP INDEX IF EXISTS idx_protected_permissions_active;
DROP TABLE IF EXISTS protected_permissions;