---
source_id: kb-privacy-transfers
title: Privacy consent and cross-border data transfers
domain: Governance Platform
module_id: privacy-transfers
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I13,I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Privacy & Cross-Border Data Transfer Governance

The privacy area records consent, privacy notices and preferences, and governs when personal data may be transferred to another country. A cross-border data transfer needs a legal transfer mechanism, a registered processor relationship and a transfer risk assessment, and the runtime transfer gate checks all three before data leaves.

## Record Subject Consent

Record subject consent stores a person's consent for a stated purpose, with a receipt that later decisions can be checked against.
To record consent:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open the **Privacy & Cross-Border Data Transfer Governance** page of the console.
3. Go to **Record Subject Consent**.
4. Enter the **Data Subject Reference**, the **Purpose ID**, the **Consent Decision**, the **Capture Channel** and the **Linked Privacy Notice Version ID**.
5. Submit the form.
Resolve consent status shows whether consent currently holds and the evidence behind it.

## Withdraw Consent (Right to Revoke)

Withdrawing consent never deletes past evidence. It adds a permanent withdrawal receipt, and every later decision for that consent resolves to WITHDRAWN.
To withdraw consent:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open the **Privacy & Cross-Border Data Transfer Governance** page of the console.
3. Go to **Withdraw Consent (Right to Revoke)**.
4. Enter the **Consent Receipt ID** being withdrawn and the **Revocation Channel** where the person asked.
5. Submit the form.

## Subject Preference Assertion

Subject preferences, such as which channels or topics a person wants to hear about, are tracked separately from consent. Each preference is enabled, disabled or unset.

## Register Privacy Notice

Register a privacy notice records the version of the notice a person was shown, so consent can be tied to what they actually read.

## Register Transfer Mechanism

A transfer mechanism is the legal basis for sending personal data across borders: standard contractual clauses, binding corporate rules, an adequacy decision or a specific derogation.
To register a transfer mechanism:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open the **Privacy & Cross-Border Data Transfer Governance** page of the console.
3. Go to **Register Transfer Mechanism**.
4. Choose the **Mechanism Type** and enter the **Evidence Reference / Legal Instrument**.
5. Add the **Conditions & Safeguards Summary** and the **Valid Until** date.
6. Select **Create Mechanism**.

## Register Processor Relationship

A processor relationship records which controller hands data to which processor, for what service and under which data processing agreement.
To register a processor relationship:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open the **Privacy & Cross-Border Data Transfer Governance** page of the console.
3. Go to **Register Processor Relationship**.
4. Enter the **Data Controller Ref**, the **Data Processor Ref**, the **Service Scope** and the **Processing Instructions**.
5. Add the **DPA Contract Reference**, the **Governing Jurisdictions**, the **Data Categories** and the **Data Subject Classes**.
6. Select **Create Processor Relationship**.
Downstream subprocessors are attached to the relationship afterwards.

## Record Transfer Risk Assessment

A transfer risk assessment records the outcome for a processor relationship: approve, remediate (human review required) or reject.
To record a transfer risk assessment:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open the **Privacy & Cross-Border Data Transfer Governance** page of the console.
3. Go to **Record Transfer Risk Assessment**.
4. Choose the **Target Relationship** and the **Evaluation Outcome**.
5. Enter the **Residual Risk Analysis**, the **Assessment Evidence Reference** and the **Review Trigger / Expiry Date**.
6. Select **Record Assessment**.

## Runtime transfer evaluation

The runtime transfer gate decides whether a transfer may happen now, and fails closed: a missing or rejected safeguard blocks the transfer.
To evaluate a transfer:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. Open the **Privacy & Cross-Border Data Transfer Governance** page of the console.
3. Go to the runtime transfer evaluation gate.
4. Choose the **Processor Relationship**, the **Transfer Mechanism** and the **Destination Jurisdiction**.
5. Choose whether to **Enforce Assessment Requirement**.
6. Select **Evaluate Transfer Decision**.
Each decision produces a receipt with its reason codes.
