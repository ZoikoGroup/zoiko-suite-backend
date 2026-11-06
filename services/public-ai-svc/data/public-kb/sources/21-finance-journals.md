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

To record a journal, enter the fiscal period (YYYY-MM), a description of why the posting exists, the journal type, such as Standard, the transaction date, the posting date, which may be later than the transaction date but never earlier, the currency (a three-letter code such as GBP), an optional accounting book, and the lines: an account code, an optional note, and a debit or credit amount. A journal moves through three stages, PENDING, VALIDATED and FINALIZED. Validation enforces double entry, so debits must equal credits. After posting a journal cannot be edited; the only correction is a reversal that posts a separate inverse entry.

## Journal register

The Journal register lists every journal for your organization, newest first, filtered by status (pending, validated, finalized or reversed), fiscal period and legal entity. Each row offers only the action that is valid from where it stands. Read one journal shows the full record: every line, each actor and time along the way, and any reversal link.

## Ingest a statement line

Ingest a statement line records what the bank says happened: the bank reference, the bank account, the statement date, the signed amount (positive is money in, negative is money out), the currency and the ledger account for that bank account. The line arrives UNMATCHED; it is then matched to a finalized journal moving money the same way, or recorded as an exception when nothing accounts for it.

## Reconciliation register

The Reconciliation register lists statement lines, newest first, with only the actions that are valid for each. MATCHED is final. A statement is reconciled per bank account and date once every line is either matched or recorded as an exception.

## Issue a customer invoice

Issue a customer invoice records money owed to you. An invoice starts ISSUED and moves to SENT, then to OVERDUE or PAID; PAID is final, and an invoice paid on time never passes through OVERDUE. Each step needs its own permission, and recording a payment also needs a finalized ledger journal linked to the invoice, so cash is never recognised against a receivable the books do not carry. The Receivables register lists customer invoices.
