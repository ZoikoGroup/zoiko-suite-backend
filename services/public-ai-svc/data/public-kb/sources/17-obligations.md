---
source_id: kb-obligations
title: Obligations
domain: Governance Platform
module_id: obligations
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I19,I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Obligations

Obligations tracks the statutory, regulatory and contractual duties of each organization: what is owed, to which jurisdiction, by when, and whether it has been discharged. Every obligation is bound to a legal entity and a jurisdiction, and carries a reference back to whatever created it, such as a contract or a filing calendar.

## How to raise an obligation

Raise an obligation records a duty against the legal entity that owes it and the jurisdiction it is owed to.
To raise an obligation:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Obligations**.
3. Go to **Raise an obligation**.
4. Enter the **Obligation code** (such as VAT-RETURN-2026-Q3), the **Obligation type** (such as FILING), the jurisdiction and the **Due date**.
5. Enter the **Severity**, the **Responsible function** (such as Finance), the **Source type**, the **Source ID** and the **Source reference**.
6. Select **Raise obligation**.
Every field is required, and the jurisdiction must already exist in Jurisdictions & Rules.

## Read this register knowing three things

Nothing moves an obligation to OVERDUE automatically when its due date passes, so the register counts past-due items separately rather than trusting the status. Obligation codes are global, and reusing a code returns the stored record instead of writing a new one.

## Obligation register

The Obligation register lists obligations for your legal entity.
To filter the register:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Obligations**.
3. Go to the **Obligation register**.
4. Choose any of **Status**, **Type**, **Due after**, **Due before** and **Scope**.
5. Select **Filter register**.

## Move an obligation's status

An obligation moves from OPEN to IN_PROGRESS, OVERDUE or CLOSED; from IN_PROGRESS to OVERDUE or CLOSED; and from OVERDUE to CLOSED.
To move an obligation's status:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Obligations**.
3. Go to **Move an obligation's status**.
4. Enter the **Obligation ID** and choose the **New status**.
5. Select **Update status**.
CLOSED is final: nothing reopens a discharged obligation, and any other move is refused.

## Record a filing requirement

A filing requirement sits beneath an obligation and records that a filing is required, never that it was made.
To record a filing requirement:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Obligations**.
3. Go to **Record a filing requirement**.
4. Enter the **Obligation ID**, the **Filing type** (such as VAT_RETURN), the **Filing authority** (such as HMRC) and the **Submission channel** (such as API).
5. Select **Record filing requirement**.
Filing requirements are recorded as PENDING and stay PENDING.

## Read a stored record

Read a stored record looks up one obligation, including when it closed and its source references, or the filing requirements recorded for it.
To read a stored record:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Obligations**.
3. Go to **Read a stored record**.
4. Enter the **Obligation ID**, or use **Filing requirements for an obligation** to list its filing requirements.
