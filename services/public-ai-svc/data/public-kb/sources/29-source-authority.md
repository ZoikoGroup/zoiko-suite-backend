---
source_id: kb-source-authority
title: Source Authority
domain: Governance Platform
module_id: source-authority
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

## Source Authority

Source Authority decides which connected system's value to trust for a field, right now. Precedence rules rank the source systems, and observations record what each one actually reported. When two equally ranked sources disagree, resolution blocks rather than guessing.

## Resolve a value

Resolve a value asks for one field on one entity and shows both the answer and the workings: every source that reported, what each said, and which rule decided between them. It only ever reads your own organization's facts.

## Precedence register

The Precedence register shows which source system outranks which, per field family, such as a payroll system over a spreadsheet. It is shared reference data, because ranking systems is a statement about the systems, not about anyone's records. Record a precedence rule adds a ranking.

## Record an observation

Record an observation stores what one source system reported, for one entity, at one moment. It is append-only and never edits an existing fact; a correction is a later observation.

## What this register will refuse

These refusals can look like failures, but they are the control working. A tie is not broken, it is reported: two equally ranked sources that disagree produce no value, so a fact nobody can vouch for stops a decision instead of quietly supplying it. A source nobody has ranked is listed alongside the resolution rather than dropped. A precedence rule cannot be edited or ended in the past; only its window closes, from now on.
