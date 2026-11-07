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

The Governance Log is the append-only evidence store behind every governed decision. Each policy evaluation is written there, and so is the basis for any action a service must be able to justify later. Entries are never edited or removed, so the log can answer who decided what, when and on which basis. A correction is made by recording a new decision, never by changing an old one.

## Decision log

The Decision log lists governed decisions, newest first, for your own organization only. Each row records a governed action being allowed, refused or sent for review, together with the rule that settled it.
To filter the Decision log:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Governance Log**.
3. Go to the **Decision log**.
4. Enter any of **Action type** (such as PAYROLL_RELEASE), **Actor**, **Legal entity** and **Rule basis** (a policy code and version).
5. Set a **From** and **To** date range and choose how many rows to show **Per page**.
Filters combine, and an invalid date is reported as an error rather than as an empty result.

## What you are looking at

Every row in the Governance Log records a governed action being allowed, refused or sent for review, together with the rule that settled it. Records are written automatically by the areas that make those decisions, nothing can be edited or deleted, and you see only your own organization's decisions. Look up a decision opens one record by its reference, and Record a decision files a decision an area must be able to justify later.

## Policies

Policies are the rules that decide whether something needs approving, and the limits they set. A policy rule is only a name: its limits live on its versions, and a version applies to nothing until it is brought into force. This keeps every change to an approval limit reviewable and dated. Only approval thresholds are acted on today; other kinds of rule can be recorded but are not yet consulted. Changing a rule needs permission, and a change that cannot be confirmed is refused rather than waved through.

## Three things this page cannot do for you

First, only approval thresholds are acted on: other kinds of rule can be created, given limits and brought into force, but the platform does not consult them yet, so a rule being in force is not the same as a rule being applied. Second, changing a rule needs permission, and if that permission check cannot be carried out the change is refused, so a refusal can mean the permission service is unreachable. Third, every check is filed in the Governance Log, but an answer on this page does not by itself prove it was recorded; look the reference up in the log when it matters.

## Check an amount

Check an amount asks whether an amount needs approving. It is compared against whichever limit is in force.
To check an amount:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. Go to **Check an amount**.
4. Enter the **Amount to check**.
5. Select **Check this amount**.
The answer is filed in the Governance Log for the record.

## What is in force right now

What is in force right now shows the rules that would decide for a kind of rule and a scope.
To see what is in force:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. Go to **What is in force right now**.
4. Choose the **Kind of rule** and **Who it applies to**.
5. Select **Show them**.
When two rules apply to the same thing, the narrowest rule decides, so the top row is the one that actually decides.

## Approval thresholds

An approval threshold is the most an action may be worth before it needs approving: anything at or under the threshold goes ahead, and anything above it is sent for approval. Thresholds are the kind of rule the platform acts on today, and they are set and brought into force in Policies.

## Create a rule

Create a rule is step 1 of 3. It names the rule, and changes nothing on its own until a limit is set and brought into force.
To create a rule:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. Go to **Create a rule**.
4. Choose the **Kind of rule** and **Who it applies to**.
5. Enter a **Short code** (such as SPEND-LIMIT-V1), which cannot be changed later, and a **Name** (such as Purchase approval threshold).
6. Select **Create the rule**.
Reusing a code with different details is refused, because it would redefine the rule rather than repeat it.

## Set the limit

Set the limit is step 2 of 3. Anything at or under the limit goes ahead, and anything above it needs approval.
To set the approval limit:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. Go to **Set the limit**.
4. Enter the **Rule reference** and the **Limit** (such as 10000).
5. Set the **Dated from** date and add any **Extra details**.
6. Select **Save the limit**.
Saving the limit does not apply it. A limit for one legal entity beats one for the whole organization, which beats one for every organization.

## Bring a limit into force

Bring a limit into force is step 3 of 3, and the only step that changes what the platform enforces.
To bring a limit into force:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. Go to **Bring a limit into force**.
4. Enter the **Rule reference** and the **Limit reference**.
5. Select **Bring it into force**.
Earlier versions stay on record, so decisions made under them keep their recorded basis.

## How to change an approval limit

Changing an approval limit means saving a new limit for the policy rule and bringing it into force.
To change an approval limit:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. In **Set the limit**, enter the **Rule reference** and the new **Limit**, set **Dated from**, and select **Save the limit**.
4. In **Bring a limit into force**, enter the **Rule reference** and the new **Limit reference**, and select **Bring it into force**.
The earlier version stays on record, so decisions made under it keep their recorded basis.

## Look up a decision

Look up a decision opens one recorded decision by its reference.
To look up a decision:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Governance Log**.
3. Go to **Look up a decision**.
4. Enter the **Decision ID** and open it.
Records are never edited, so the decision shows exactly what was recorded and on which rule.

## Record a decision

Record a decision files a governed decision that an area must be able to justify later.
To record a decision:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Governance Log**.
3. Go to **Record a decision**.
4. Enter the **Action type**, the **Outcome**, the **Rule basis**, the **Actor** and the **Legal entity**.
5. Add anything else worth recording, then select **Append to the log**.
The entry is permanent; a correction is made by recording a new decision, never by changing an old one.

## The history of one rule

The history of one rule lists every version of a rule and when each applied.
To see a rule's history:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Governance Platform**, then select **Policies**.
3. Go to **The history of one rule**.
4. Enter the **Rule reference** and select **Show the history**.
