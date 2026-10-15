-- Migration: 000007_add_negative_stock_policy.up.sql
--
-- INV-01 owns the item's negative-stock policy. Spec (Domain Constitution,
-- invariant 11): "Negative inventory behavior is explicit policy; the system
-- never silently allows negative stock because a projection is stale." The
-- INV-01 "Server-resolved/derived context" field names "negative-stock
-- policy" as one of the effective-dated profile inputs.
--
-- Carried on inventory_tracking_policies so it is versioned and effective-
-- dated exactly like lot/serial/expiry tracking — never edited in place, a
-- new SetTrackingPolicy version is the only way to change it. Default is
-- PROHIBITED, the safe direction: every existing item and every policy
-- version created without the field keeps today's behavior (INV-03 refuses
-- any movement that would take on-hand below zero).
ALTER TABLE inventory_tracking_policies
    ADD COLUMN negative_stock_policy VARCHAR(20) NOT NULL DEFAULT 'PROHIBITED',
    ADD CONSTRAINT chk_inventory_negative_stock_policy
        CHECK (negative_stock_policy IN ('PROHIBITED', 'ALLOWED'));
