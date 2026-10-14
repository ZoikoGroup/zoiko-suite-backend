-- Migration: 000300_ar03_sales_orders.down.sql
DROP TABLE IF EXISTS ar03_order_history;
DROP TABLE IF EXISTS ar03_credit_exceptions;
DROP TABLE IF EXISTS ar03_order_approvals;
DROP TABLE IF EXISTS ar03_invoiced_ledger;
DROP TABLE IF EXISTS ar03_billing_milestones;
DROP TABLE IF EXISTS ar03_order_lines;
DROP TABLE IF EXISTS ar03_order_versions;
DROP TABLE IF EXISTS ar03_sales_orders;
DROP TABLE IF EXISTS ar03_order_counters;
DROP FUNCTION IF EXISTS ar03_guard_milestones();
DROP FUNCTION IF EXISTS ar03_guard_lines();
DROP FUNCTION IF EXISTS ar03_check_version_seq();
DROP FUNCTION IF EXISTS ar03_guard_orders();
