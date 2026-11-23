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

Check a permission asks exactly what a service asks before it acts: may this person take this action for this legal entity? The answer is recorded in the decision log.
To check whether someone has access:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Go to **Check a permission**.
4. Enter the **Action** (such as invoice.approve), the **Person (principal ID)**, **Where** and the **Legal entity**.
5. Optionally add the **Amount**, **Currency**, **Owner of the record** and **Other attributes**.
6. Select **Check**.

## Maker and checker

Maker and checker asks whether the same people may both do and approve one act. It is a question only and records nothing.
To run the check:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Go to **Maker and checker**.
4. Enter the **Action** and the **Maker (principal ID)**, and optionally the **Checker** and **Who it is about**.
5. Select **Check**.

## Where can someone act?

Where can someone act lists the legal entities a person can act in. It records nothing.
To find out where someone can act:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Go to **Where can someone act?**.
4. Enter the **Person**, or leave it blank for yourself, and optionally an **Action**.
5. Select **Check**.

## Borrowed authority?

Borrowed authority asks whether a person is acting on their own authority or on someone else's through a delegation.
To check for borrowed authority:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Go to **Borrowed authority?**.
4. Enter the **Person (principal ID)**, the **Legal entity**, the **Action** and, optionally, who it was **Lent by**.
5. Select **Check**.
The answer says which actions are borrowed, the delegation they come from, and whether the person also holds the action directly.

## Effective access

Effective access shows everything a person can do right now, recomputed from their role grants, delegations and elevated rights.
To see effective access:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Go to **Effective access**.
4. Enter the **Person (principal ID)**, or leave it blank for yourself.
5. Select **Recompute**.

## Actions on a record and Before granting

Actions on a record shows which actions a person may take on one specific record. Before granting checks whether a role grant would create a separation-of-duties conflict before it is made.
To see the actions on a record:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Authorization**.
3. Go to **Actions on a record**.
4. Enter the **Record type**, the **Record ID**, the **Person** and the **Legal entity**.
5. Select **Show actions**.
