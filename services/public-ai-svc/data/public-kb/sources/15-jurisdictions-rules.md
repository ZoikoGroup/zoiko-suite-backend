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

Register a jurisdiction adds a jurisdiction to the register that obligations, rules and legal entities are checked against.
To register a jurisdiction:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Jurisdictions & Rules**.
3. Go to the **Register a jurisdiction** section.
4. Enter the **Code** (such as GB-SCT), the **Name** (such as Scotland), the **Type** (such as country) and the **Authority**.
5. Set the **Effective from** date, and an **Effective to** date if the jurisdiction should end.
6. In **Nested in**, choose the parent jurisdiction, or leave it as a root jurisdiction.
7. Select **Register jurisdiction**.
Only a role allowed to create jurisdictions for the whole platform can register one. Submitting the same details again changes nothing, and reusing a code with different details is refused.

## How to record a rule

Record a rule attaches an applicability rule to one jurisdiction. It applies there and in every jurisdiction nested inside it, unless a nested jurisdiction declares the same rule code itself.
To record a rule:
1. Sign in to your ZoikoSuite account.
2. In the left menu, open **Governance Platform**, then select **Jurisdictions & Rules**.
3. Go to the **Record a rule** section.
4. Choose the **Jurisdiction**, then enter the **Domain** (such as TAX), the **Rule code** (such as VAT-REGISTRATION) and a **Name**.
5. Set the **Effective from** date and the **Status**. A DRAFT rule is registered but not yet applied.
6. Optionally add the **Source reference** (the legislation the rule encodes) and the **Applicability payload**.
7. Select **Record rule**.
One rule wins per domain and rule code when a rule pack is resolved.

## How to resolve a rule pack

Resolve a rule pack shows what actually applies in a jurisdiction on a date.
To resolve a rule pack:
1. Sign in to your ZoikoSuite account.
2. In the left menu, open **Governance Platform**, then select **Jurisdictions & Rules**.
3. Go to the **Resolve a rule pack** section.
4. Choose the **Jurisdiction** and, optionally, the **As at** date.
5. Select **Resolve rule pack**.
Rules are collected from the jurisdiction and every parent, then narrowed so exactly one wins per domain and code: the most specific jurisdiction first, then the latest effective-from date. The chain the answer was built from is shown with it.

## Deactivating a jurisdiction

A jurisdiction is never deleted. Deactivating it end-dates it; it stays visible, and anything that checks against the register then refuses new records for it.
To deactivate a jurisdiction:
1. Sign in to your ZoikoSuite account.
2. In the left menu, open **Governance Platform**, then select **Jurisdictions & Rules**.
3. Find the jurisdiction in the **Jurisdiction register**.
4. Select **Deactivate** on that jurisdiction.
Records already bound to the jurisdiction keep their history.
