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

Raise an obligation against the legal entity that owes it and the jurisdiction it is owed to. Enter an obligation code, such as VAT-RETURN-2026-Q3, the obligation type, such as FILING, the jurisdiction, the due date, the severity, the responsible function, such as Finance, the source type and source ID, and a source reference. Every field is required, because a duty without a deadline, an owner or a traceable source cannot be acted on. The jurisdiction must already exist in Jurisdictions & Rules, so an obligation can never point at a jurisdiction that is not recorded.

## Read this register knowing three things

Nothing moves an obligation to OVERDUE automatically when its due date passes, so the register counts past-due items separately rather than trusting the status. The legal entity filter narrows the view; it is not an isolation boundary. Obligation codes are global, and reusing a code returns the stored record instead of writing a new one.

## Obligation register

The Obligation register lists obligations for your legal entity. Filter it by status, type, due-after and due-before dates and scope.

## Move an obligation's status

An obligation moves from OPEN to IN_PROGRESS, OVERDUE or CLOSED; from IN_PROGRESS to OVERDUE or CLOSED; and from OVERDUE to CLOSED. CLOSED is final: nothing reopens a discharged obligation. Asking for the status an obligation already has changes nothing, and any other move is refused.

## Record a filing requirement

A filing requirement sits beneath an obligation: choose the obligation, the filing type, such as VAT_RETURN, the filing authority, such as HMRC, and the submission channel, such as API. Filing requirements are recorded as PENDING and stay PENDING, so they record that a filing is required, never that it was made.

## Read a stored record

Read a stored record looks up one obligation by its ID, including when it closed and its source references, or lists the filing requirements recorded for an obligation.
