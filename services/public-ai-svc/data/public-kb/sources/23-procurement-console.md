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

Screen a counterparty runs a due-diligence check on a vendor or other counterparty. The outcome and its evidence are written together, and a no-match is not a clearance.
To screen a counterparty:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Commercial Ops**.
3. Go to **Screen a counterparty**.
4. Enter the **Vendor name**, the **Counterparty ID** and a **Document reference**.
5. Select **Screen this counterparty**.
The Screening register keeps every run, newest first, as the audit trail.

## Screening register

The Screening register lists every check for your organization, newest first: each run, not one row per counterparty, because the history is the audit trail. Flagged is a finding, and the counterparty is rejected as high risk. Screened, no match means a comparison ran and found nothing, which is recorded but is not an approval. No outcome or Failed means the counterparty has not been screened at all.

## Spend controls

Spend controls set the limits a purchase is checked against, per category and currency.
To set a spend limit:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Commercial Ops**.
3. Go to **Spend controls**.
4. Enter the **Category**, the **Enforcement window**, the **Limit** and the **Currency**.
5. Select **Set limit**.
Limits in force lists the limits that currently apply.

## Check a spend before committing it

Check a spend before committing it has four outcomes: permitted, refused, not evaluated (no limit for that category, so not an approval), or refused for a currency other than the limit's.
To check a spend:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Commercial Ops**.
3. Go to **Check a spend before committing it**.
4. Enter the **Category**, the **Proposed amount**, the **Currency** and a **Source reference**.
5. Select **Check this spend**.
A permitted spend is recorded against the budget straight away; a refused one uses none of it.

## Issue a purchase order

Issue a purchase order raises an order against a supplier for an approved purchase request.
To issue a purchase order:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Commercial Ops**.
3. Go to **Issue a purchase order**.
4. Enter the **Order total**, the **Currency**, the **Purchase request ID** and the **Vendor profile ID**.
5. Select **Issue purchase order**.
Amending an order later restates its total with a reason and appends an amendment record; it never overwrites history.

## Order register

The Order register lists every order for your organization. Amending an order restates its total and appends a permanent amendment record, and closing an order is final.
To find or amend an order:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Commercial Ops**.
3. Go to the **Order register**, enter the **Legal entity** and select **Filter orders**.
4. To amend an order, enter the **Revised total** and a **Reason**.
5. Use **Read one order** with the **Purchase order ID** to see the full record.

## Amendment ledger

The Amendment ledger is append-only, oldest first. Each row is one restatement: the version it moved from and to, the total before and after, and the reason given.
To read an order's amendments:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Legal, Corporate & Commercial**, then select **Commercial Ops**.
3. Go to the **Amendment ledger**.
4. Enter the **Purchase order ID**.
Nothing in the ledger can be edited or deleted.
