-- Migration: 000700_ar07_receivables.down.sql
DROP TABLE IF EXISTS ar07_item_history;
DROP TABLE IF EXISTS ar07_disputes;
DROP TABLE IF EXISTS ar07_adjustments;
DROP TABLE IF EXISTS ar07_open_items;
DROP FUNCTION IF EXISTS ar07_guard_adjustment();
DROP FUNCTION IF EXISTS ar07_guard_open_item();
DROP FUNCTION IF EXISTS ar07_guard_open_item_insert();
