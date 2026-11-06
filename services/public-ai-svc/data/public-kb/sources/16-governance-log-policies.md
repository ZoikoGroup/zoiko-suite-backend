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

The Decision log lists governed decisions, newest first, for your own organization only. Each row records a governed action being allowed, refused or sent for review, together with the rule that settled it. You can filter it by action type, such as PAYROLL_RELEASE, by the person who acted, by legal entity, by the rule basis (a policy code and version) and by a date range, then choose how many rows to show per page. Filters combine, and an invalid date is reported as an error rather than as an empty result.

## Policies

Policies are the rules that decide whether something needs approving, and the limits they set. A policy rule is only a name: its limits live on its versions, and a version applies to nothing until it is brought into force. This keeps every change to an approval limit reviewable and dated. Only approval thresholds are acted on today; other kinds of rule can be recorded but are not yet consulted. Changing a rule needs permission, and a change that cannot be confirmed is refused rather than waved through.

## What is in force right now

What is in force right now shows the rules that would decide for a kind of rule and a scope, such as this organization. Choose the kind of rule and who it applies to, then show them. The narrowest rule wins, so the top row is the one that actually decides.

## Create a rule

Create a rule is step 1 of 3. Enter a short code, such as SPEND-LIMIT-V1, which cannot be changed later, a name people will recognise, such as Purchase approval threshold, and the kind of rule. Creating a rule changes nothing on its own: it has no limit until you set one. Reusing a code with different details is refused, because it would redefine the rule rather than repeat it.

## Set the limit

Set the limit is step 2 of 3. Choose the rule by its reference, enter the limit, such as 10000, a dated-from date and who it applies to. Anything at or under the limit goes ahead and anything above it needs approval. A limit set for one legal entity beats one set for the whole organization, which in turn beats one set for every organization. Saving the limit does not apply it.

## Bring a limit into force

Bring a limit into force is step 3 of 3, and the only step that changes what the platform enforces. Until a saved limit is brought into force it applies to nothing.

## How to change an approval limit

Create a new version of the policy rule with the new limit, then bring that version into force. The earlier version stays on record, so decisions made under it keep their recorded basis.
