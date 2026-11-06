---
source_id: kb-tenants-entities
title: Tenants and legal entities
domain: Governance Platform
module_id: tenants-entities
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

## Tenants

A tenant is your organization in ZoikoSuite. Each tenant holds its own legal entities, workspaces and settings, and nothing in one tenant is visible to another tenant.

## This tenant

This tenant shows your own organization's record. Status and lifecycle are separate: a new tenant is ACTIVE in status while still ONBOARDING in its lifecycle, and only the lifecycle moves through defined stages.

## Provision a tenant

Provision a tenant creates a new organization. It is permitted at platform level, because there is no tenant yet to check it against.
To provision a tenant:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Provision a tenant**.
4. Enter the **Tenant code**, the **Legal name** and the **Trading name**.
5. Set the **Default currency**, **Primary timezone** and **Primary locale**.
6. Select **Provision tenant**.
A new tenant is ACTIVE in status while still ONBOARDING in its lifecycle.

## Lifecycle commands and history

Lifecycle commands move a tenant through its lifecycle with named governance commands, which are preferred because the history then records which command was used. Lifecycle history lists everything that has happened to the tenant.

## Hostnames

Hostnames lists the web addresses that resolve to your organization.
To bind a hostname:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Lifecycle evidence & hostnames**.
4. Enter the **Hostname**.
5. Select **Bind hostname**.

## Create a legal entity

Create a legal entity adds a company to your organization.
To add a legal entity:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Create a legal entity**.
4. Enter the **Entity code**, the **Legal name** and the **Entity type**.
5. Choose the **Default currency**, the **Fiscal calendar ID**, the **Data residency policy** and the **Primary jurisdiction**.
6. Select **Create entity**.
Entity type, jurisdiction, fiscal calendar, residency policy and entity code are fixed at creation, because posted transactions reference them.

## Update an entity

Update an entity changes only the descriptive fields; the fields posted transactions reference are fixed at creation.
To update an entity:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Update an entity**.
4. Choose the **Entity**, then enter the **Legal name**, a **New trading name** or the **Default currency**.
5. Select **Update entity**.
A blank field means leave it alone, so clearing a trading name is an explicit choice.

## Entity hierarchy

Entity hierarchy shows parent and child relationships between your legal entities, effective-dated. The same pair may hold several relationship types at once.
To add a relationship:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Entity hierarchy**.
4. Choose the **Parent entity** and **Child entity**, the **Relationship type** and **Effective from**.
5. Select **Create relationship**.

## Create a data residency policy

Data residency says which hosting region an organization's data lives in. A new tenant starts with a default setting and no region, so its hosting region reads as unresolved until one is assigned.
To create a data residency policy:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Create a data residency policy**.
4. Enter the **Policy name** and **Policy code**, and choose the **Residency mode**, the **Conflict resolution** and the **Residency region**.
5. Select **Create policy**.

## Workspaces

A workspace sits beneath the tenant and may be scoped to one legal entity. Its billing classification decides whether it can ever produce a live charge.
To create a workspace:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Create a workspace**.
4. Enter the **Name** and **Business unit**, and choose the **Billing classification**, the **Billing source** and the **Legal entity scope**.
5. Select **Create workspace**.
An unrecognised billing classification is refused.

## Registry integrity

Two active entities in one jurisdiction cannot hold the same registration number. An attempt to create one is refused and quarantined with its details intact, never merged.
To find an entity by registry number:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Registry integrity**.
4. Enter the **Registration number** and the **Jurisdiction**.
5. Select **Search**.
Quarantined duplicates are reviewed with a **Conclusion** and a **Note**, then **Record conclusion**.

## Quarantined duplicates

Quarantined duplicates lists refused attempts to create an entity whose registration number is already held in that jurisdiction, with their details kept intact for review.

## Legal profile versions

An entity's legal profile is versioned, so every change is kept.

## Reconstruct as of a date

Reconstruct as of a date shows an entity's legal profile as it stood on any earlier date, from its version history.
To reconstruct a profile:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Legal profile versions**.
4. Choose the **Legal entity** and the **As of** date.
5. Select **Reconstruct**.

## Assign a jurisdiction

Assign a jurisdiction links a legal entity to a jurisdiction registered in Jurisdictions & Rules.
To assign a jurisdiction:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Tenants & Entities**.
3. Go to **Assign a jurisdiction**.
4. Choose the **Entity** and the **Jurisdiction**, the **Assignment type**, **Effective from** and the **Source basis**.
5. Select **Assign jurisdiction**.
