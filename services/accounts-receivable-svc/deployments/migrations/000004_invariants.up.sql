-- Migration: 000004_invariants.up.sql
--
-- Writes the invoice's invariants into the schema. None of these were there: the
-- table has always been a set of loosely-typed columns with the rules living only
-- in Go, so anything that reached this database by another route -- a fix-up
-- script, a future service, a psql session -- could leave a receivable the domain
-- considers impossible.
--
-- Every constraint below is added NOT VALID, deliberately. NOT VALID enforces the
-- constraint on every INSERT and UPDATE from here on; what it skips is the scan
-- that would refuse to apply the migration at all if an EXISTING row violated it.
-- These rows are a record of what customers were billed. A migration that will not
-- apply, or that rewrites history to fit a rule written afterwards, is worse than
-- one that constrains everything from now on and leaves the past legible. Run
-- ALTER TABLE ... VALIDATE CONSTRAINT afterwards to prove the backlog is clean.
-- Same reasoning, and the same pattern, as board-resolutions-svc's 000002.

-- The four lifecycle states, and only those. The service is the only writer today,
-- so this is defence in depth rather than a fix -- but a status the domain cannot
-- name is a row no code path can correctly handle, and the register's status filter
-- now refuses unknown values on the way in, which would be pointless if the column
-- accepted them from elsewhere.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_status_valid
    CHECK (status IN ('ISSUED', 'SENT', 'OVERDUE', 'PAID')) NOT VALID;

-- An invoice for nothing, or for a negative sum, is not an invoice. The handler
-- has always refused amount <= 0; the column has always accepted it.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_amount_positive
    CHECK (amount > 0) NOT VALID;

-- ISO 4217 is three uppercase letters. VARCHAR(3) bounds the length and nothing
-- else, so 'gb', '£', and '123' all fit. Currency is not checked against a
-- registry anywhere in this platform -- there is no currency service -- so the
-- shape is all that can honestly be enforced here.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_currency_code_shape
    CHECK (currency_code ~ '^[A-Z]{3}$') NOT VALID;

-- Attribution must accompany the state it explains, in both directions.
--
-- The lifecycle stamps a principal and a timestamp per hop, and the pairs are
-- meaningless apart: an actor with no time says nothing about when, and a time
-- with no actor is an event nobody is answerable for. Both halves are also
-- REQUIRED by the status: a PAID invoice with no payment_received_by_principal_id
-- is money recorded as received on nobody's authority, which is exactly the shape
-- an audit exists to catch.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_sent_attribution_paired
    CHECK ((sent_by_principal_id IS NULL) = (sent_at IS NULL)) NOT VALID;

ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_overdue_attribution_paired
    CHECK ((marked_overdue_by_principal_id IS NULL) = (marked_overdue_at IS NULL)) NOT VALID;

ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_payment_attribution_paired
    CHECK ((payment_received_by_principal_id IS NULL) = (payment_received_at IS NULL)) NOT VALID;

-- A status implies the hops that must already have happened.
--
-- SENT, OVERDUE and PAID are all reachable only through SENT, so each of them
-- requires the send to be stamped. PAID additionally requires its own stamp.
-- OVERDUE is NOT required for PAID -- an invoice paid on time never passes through
-- it -- which is the branch in this lifecycle and the reason this is three
-- conditions rather than a linear ladder.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_sent_before_later_states
    CHECK (status = 'ISSUED' OR sent_at IS NOT NULL) NOT VALID;

ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_overdue_stamped
    CHECK (status <> 'OVERDUE' OR marked_overdue_at IS NOT NULL) NOT VALID;

ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_paid_stamped
    CHECK (status <> 'PAID' OR payment_received_at IS NOT NULL) NOT VALID;

-- An invoice cannot be declared overdue before its due date has passed. This is
-- the same rule the handler enforces (see the overdue check), written where it
-- cannot be bypassed. The due date is the last day payment is on time, so the
-- invoice turns overdue at the start of the following day.
ALTER TABLE customer_invoices
    ADD CONSTRAINT customer_invoices_overdue_after_due_date
    CHECK (marked_overdue_at IS NULL OR marked_overdue_at >= (due_date + INTERVAL '1 day')) NOT VALID;
