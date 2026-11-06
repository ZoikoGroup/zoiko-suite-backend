---
source_id: kb-jurisdictions-rules
title: Jurisdictions & Rules
domain: Governance Platform
module_id: jurisdictions-rules
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

## Jurisdictions & Rules

Jurisdictions & Rules is the register every other area defers to for which law applies where. Jurisdictions nest, for example a country and its regions, their rules are effective-dated, and legal drift is tracked separately from whether a rule is in force. Until a jurisdiction is registered, an obligation cannot be bound to it.

## How to register a jurisdiction

Open Governance Platform, then Jurisdictions & Rules, and use Register a jurisdiction. Enter a short code, such as GB-SCT, a name, such as Scotland, the type, such as country, the authority, and the effective-from date, with an optional effective-to date. Registering is a governed, platform-level action: only a role allowed to create jurisdictions for the whole platform can do it. Submitting the identical details again changes nothing, and reusing a code with different details is refused, because other records are already bound to it.

## How to resolve a rule pack

Resolve a rule pack shows what actually applies in a jurisdiction on a date. Choose the jurisdiction and, optionally, the as-at date. Rules are collected from the jurisdiction and every parent, then narrowed so exactly one wins per domain and code: the most specific jurisdiction first, then the latest effective-from date. The chain the answer was built from is shown with it, so a governed decision keeps its basis on record.

## Deactivating a jurisdiction

A jurisdiction is never deleted. Deactivating it end-dates it; it stays visible, and anything that checks against the register then refuses new records for it.
