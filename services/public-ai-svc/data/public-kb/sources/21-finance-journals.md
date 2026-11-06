---
source_id: kb-finance-journals
title: Finance & Payments journals and bank reconciliation
domain: Finance
module_id: finance-journals
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

## Record a journal

Record a journal posts to the general ledger. A journal moves through PENDING, VALIDATED and FINALIZED; validation enforces double entry, so debits must equal credits, and after posting it cannot be edited: to reverse a journal, post a reversal, a separate inverse entry.
To record a journal:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to **Record a journal**.
4. Enter the **Fiscal period** (YYYY-MM), a **Description**, the **Journal type**, the **Transaction date**, the **Posting date** and the **Currency**, and optionally the **Accounting book**.
5. Add the lines: an **Account code**, an optional **Line note**, and a **Debit** or **Credit** amount.
6. Select **Record journal**.
The posting date may be later than the transaction date but never earlier, and a closed period refuses new postings.

## Journal register

The Journal register lists every journal for your organization, newest first, and each row offers only the action that is valid from where it stands.
To find a journal:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to the **Journal register**.
4. Enter the **Fiscal period** and **Legal entity**, and select **Filter journals**.
5. Use **Read one journal** to see every line, each actor and time, and any reversal link.

## Ingest a statement line

Ingest a statement line records what the bank says happened, so it can be matched to the ledger.
To ingest a statement line:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to **Ingest a statement line**.
4. Enter the **Bank reference**, the **Bank account ID**, the **Statement date**, the **Amount** (positive is money in, negative is money out) and the **Currency**.
5. Enter the **Ledger account for this bank account**.
6. Select **Ingest statement line**.
The line arrives UNMATCHED; it is then matched to a finalized journal or recorded as an exception.

## Reconciliation register

The Reconciliation register lists statement lines, newest first. MATCHED is final, and a statement is reconciled per bank account and date once every line is matched or recorded as an exception.
To work the register:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to the **Reconciliation register**.
4. Enter the **Bank account** and **Statement date**, and select **Filter statement lines**.
5. Match each line to a **Journal ID**, or record it as an exception with a **Reason**.
6. When every line is settled, select **Declare reconciled**.

## Issue a customer invoice

Issue a customer invoice records money owed to you. An invoice starts ISSUED and moves to SENT, then to OVERDUE or PAID; PAID is final.
To issue a customer invoice:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to **Issue a customer invoice**.
4. Enter the **Customer reference**, the **Invoice number**, the **Due date**, the **Amount** and the **Currency**.
5. Select **Issue invoice**.
Recording a payment needs a finalized ledger journal linked to the invoice. The Receivables register lists customer invoices.

## Record a vendor invoice

Record a vendor invoice enters a supplier's invoice into payables.
To record a vendor invoice:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to **Record a vendor invoice**.
4. Enter the **Vendor reference**, the **Vendor's invoice number**, the **Due date**, the **Amount** and the **Currency**.
5. Select **Record invoice**.
The Payables register shows the stage each supplier invoice has reached.

## Register a fiscal period

Fiscal periods are registered here before postings are made in them.
To register a fiscal period:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Finance & Treasury**, then select **Finance & Payments**.
3. Go to **Register a fiscal period**.
4. Enter the **Period name**, when it **Starts** and when it **Ends**.
5. Select **Register period**.
The register shows which periods are open for posting and which are sealed.
