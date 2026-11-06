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
intent_ids: I02
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Privacy & Cross-Border Data Transfer Governance

The privacy area records consent, privacy notices and preferences, and governs when personal data may be transferred to another country. A cross-border data transfer needs a legal transfer mechanism, a registered processor relationship and a transfer risk assessment, and the runtime transfer gate checks all three before data leaves.

## Record Subject Consent

Record subject consent stores a person's consent for a stated purpose, with a receipt that later decisions can be checked against. Resolve consent status shows whether consent currently holds and the evidence behind it.

## Withdraw Consent (Right to Revoke)

Withdrawing consent never deletes past evidence. It adds a permanent withdrawal receipt, and every later decision for that consent resolves to WITHDRAWN. Record the consent receipt being withdrawn and the channel where the person asked.

## Subject Preference Assertion

Subject preferences, such as which channels or topics a person wants to hear about, are tracked separately from consent. Each preference is enabled, disabled or unset.

## Register Privacy Notice

Register a privacy notice records the version of the notice a person was shown, so consent can be tied to what they actually read.

## Register Transfer Mechanism

A transfer mechanism is the legal basis for sending personal data across borders: standard contractual clauses, binding corporate rules, an adequacy decision or a specific derogation. Record its evidence reference, the conditions and safeguards, and when it expires.

## Register Processor Relationship

A processor relationship records which controller hands data to which processor, for what service, under which data processing agreement, in which jurisdictions, and for which data categories and people. Downstream subprocessors are attached to it.

## Record Transfer Risk Assessment

A transfer risk assessment records the outcome for a processor relationship: approve, remediate (human review required) or reject. It includes the residual risk analysis, an evidence reference and a review date.

## Runtime transfer evaluation

The runtime transfer gate decides whether a transfer may happen now. It checks the relationship's status, whether the safeguard is still valid, and the risk assessment outcome, and it fails closed: a missing or rejected safeguard blocks the transfer. Each decision produces a receipt with its reason codes.
