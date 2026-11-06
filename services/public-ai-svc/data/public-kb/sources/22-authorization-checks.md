---
source_id: kb-authorization-checks
title: Authorization checks and access questions
domain: Governance Platform
module_id: authorization-checks
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

## What you can do

What you can do lists your own permissions in your current legal entity, as the authorization service resolves them, so you can see what you are allowed to do before you try it.

## Check a permission

Check a permission asks exactly what a service asks before it acts: may this person take this action, such as approving an invoice, for this legal entity? The answer is recorded in the decision log.

## Maker and checker

Maker and checker asks whether the same people may both do and approve one act. Enter the action, the person making it, and optionally the checker and the person it is about. It is a question only and records nothing.

## Where can someone act?

Where can someone act lists the legal entities a person can act in, optionally for one action. Leave the person blank to ask about yourself. It records nothing.

## Borrowed authority?

Borrowed authority asks whether a person is acting on their own authority or on someone else's through a delegation. The answer says which actions are borrowed, the delegation they come from, and whether the person also holds the action directly.

## Effective access

Effective access shows everything a person can do right now, recomputed from their role grants, delegations and elevated rights. Leave the person blank to see your own.

## Actions on a record and Before granting

Actions on a record shows which actions a person may take on one specific record. Before granting checks what a role grant would add and whether it would create a separation-of-duties conflict, so a conflict is found before the grant is made rather than after.
