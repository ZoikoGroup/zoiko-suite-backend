---
source_id: kb-governance-log-policies
title: Governance Log and Policies
domain: Governance Platform
module_id: governance-log-policies
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

## Governance Log

The Governance Log is the append-only evidence store behind every governed decision. Each policy evaluation is written there, and so is the basis for any action a service must be able to justify later. Entries are never edited or removed, so the log can answer who decided what, when and on which basis.

## Policies

Policies are the rules that decide whether something needs approving, and the limits they set. A policy rule is only a name: its limits live on its versions, and a version applies to nothing until it is brought into force. This keeps every change to an approval limit reviewable and dated.

## How to change an approval limit

Create a new version of the policy rule with the new limit, then bring that version into force. The earlier version stays on record, so decisions made under it keep their recorded basis.
