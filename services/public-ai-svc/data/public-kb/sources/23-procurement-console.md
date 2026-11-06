---
source_id: kb-procurement-console
title: Commercial Ops screening, spend controls and purchase orders
domain: Commercial Ops
module_id: procurement-console
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Screen a counterparty

Screen a counterparty runs a due-diligence check on a vendor or other counterparty. The outcome and the evidence supporting it are written together, so a conclusion can never outlive its evidence. A no-match is not a clearance. Starting a screening and reading the register are separate permissions.

## Screening register

The Screening register lists every check for your organization, newest first: each run, not one row per counterparty, because the history is the audit trail. Flagged is a finding, and the counterparty is rejected as high risk. Screened, no match means a comparison ran and found nothing, which is recorded but is not an approval. No outcome or Failed means the counterparty has not been screened at all.

## Spend controls

Spend controls set the limits a purchase is checked against, per category and currency. Limits in force lists the limits that currently apply.

## Check a spend before committing it

Check a spend before committing it has four outcomes. Permitted means a limit was evaluated, the spend fits, and it is recorded against the budget straight away. Refused means the limit said no, which is the control working, and it uses none of the budget. Not evaluated means the category has no limit at all, so nothing was checked and it is not an approval. A spend in a different currency from the limit is refused rather than converted.

## Issue a purchase order

Issue a purchase order raises an order against a supplier for an approved need. Every order is recorded for your organization and legal entity.

## Order register

The Order register lists every order for your organization, filtered by legal entity. Amending an order restates its total and appends a permanent amendment record; it never overwrites history and does not change the order's status. Closing an order is final. Read one order shows the full record.

## Amendment ledger

The Amendment ledger is append-only, oldest first. Each row is one restatement: the version it moved from and to, the total before and after, and the reason given. Nothing in it can be edited or deleted. An unknown order is reported as absent, while an order with no amendments shows an empty ledger.
