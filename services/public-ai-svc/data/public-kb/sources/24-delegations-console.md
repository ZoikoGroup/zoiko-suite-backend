---
source_id: kb-delegations-console
title: Delegation register
domain: Governance Platform
module_id: delegations-console
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

## Delegation register

The Delegation register lists every delegation you are entitled to see. With permission to view delegations for a legal entity it is that entity's full register; without it, you see the delegations you are personally party to. A delegation that has reached its end date shows as EXPIRED when the register is read.

## Grant a delegation

Grant a delegation hands one of your own authorities to another person for a fixed window. The platform first confirms that the delegator holds the action, so a delegation never creates new authority.
To delegate your approval authority:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Delegated Authority**.
3. Go to **Grant a delegation**.
4. Enter the **Delegator**, the **Delegate**, the **Action type** and the **Legal entity**.
5. Set **Effective from** and **Effective to**.
6. Select **Grant delegation**.
Submitting the same form twice repeats the first grant rather than creating a second.

## What this register will refuse

These refusals can look like permission problems, but they are the control working. You may only delegate your own authority; arranging a delegation between other people needs administration rights on the legal entity. An administrator may not be the beneficiary of a delegation they arrange. A delegation cannot exceed its delegator: the platform confirms the delegator holds the action before writing anything.

## Revoke a delegation

A delegation can be revoked at any time, and it lapses on its own when its window ends.
To revoke a delegation:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Delegated Authority**.
3. Find the delegation in the **Delegation register**.
4. Select **Revoke**.
Pending approvals that relied on it are checked again.
