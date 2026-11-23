---
source_id: kb-access-settings-console
title: Sign-in security, gateway, secrets, settings and notifications
domain: Governance Platform
module_id: access-settings-console
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids:
market_codes: ALL
classification: PUBLIC_APPROVED
pii_state: CLEAR
license: OWNED_PUBLIC
signing_ref: kbctl-dev
---

## Sign-in security

Sign-in security shows your own second factors: what protects your sign-in and what proves it is you before a high-risk change. Passkeys work only on the site they are bound to, so a look-alike page gets nothing; authenticator app codes change frequently but can be phished. Adding or removing a factor needs a fresh second factor, a code is accepted once, and an administrator's reset removes every factor.

## What each refusal means

When the gateway refuses a request it names its reason, and the same status code can mean different things depending on that reason, such as an expired session, a missing second factor or an unknown client. Would the gateway let me through checks your own session against the gateway's rules. Gateway health shows whether the gateway is serving.

## How access actually works

Secret access takes four steps, and an unconfigured secret is an inaccessible one: the platform refuses rather than guessing.
To provision a secret:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Secret Vault**.
3. Register the path: enter the **Scope**, **Secret class**, **Data classification** and **Secret path**, then select **Register path**.
4. Add a version: enter the **Secret policy ID**, **Max lease duration**, **Effective from** and **Allowed workloads**, then select **Create draft version**.
5. Enter the **Version ID** and select **Activate version**; until then the secret is invisible.
6. Enter the **Secret material** and select **Store material**.

## Version history and issued leases

Version history shows every version of one secret policy, which is the fastest way to see whether a version was created but never activated. Issued leases lists the grants only; a refused request never becomes a lease and appears in the access audit log instead. Provision a secret and Rotate a secret add new material without exposing the old.

## Settings

Settings brings together your current session, your identity and the platform configuration: features that can be switched on and off, and the named values services read at runtime.

## Settings services read at runtime

Settings services read at runtime are named values the platform reads to decide how to behave, such as a cutoff hour, a limit or an address.
To change a setting:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Feature Flags & Config**.
3. Go to **Settings services read at runtime**.
4. Choose **Which setting**, **Where it applies** and **Who it applies to**, and enter **What it should be set to**.
5. Select **Save this setting**.
Nothing is overwritten: every earlier value stays in the history.

## Check exactly what a service would see

Check exactly what a service would see answers one precise question: for this name, this environment and this group, what is in force?
To check one setting:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Feature Flags & Config**.
3. Go to **Check exactly what a service would see**.
4. Choose **Is a feature switched on?** or **What is a setting set to?**, and enter the name, environment and group.
Finding nothing means the setting is unset for that combination only, not everywhere.

## Delivery register

The Delivery register lists a legal entity's notifications, newest first. Each row records the address it was actually delivered to and where that address came from. RETRYING rows have not failed: another attempt is scheduled. FAILED rows are the ones worth acting on, because they are recorded proof that the notice did not go out. Send a notification sends one to a person through a chosen channel.

## Retire, reactivate, or rename

Retiring a role is a real control: a retired role grants nothing on the next permission check. It is reversible, because assignments are left intact.
To retire, reactivate or rename a role:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Roles & Access**.
3. Go to **Retire, reactivate, or rename**.
4. Choose the **Role** and its new **Status**, or enter a **New display name**.
5. Select **Apply change**.

## Would the gateway let me through?

Would the gateway let me through checks your own session against the gateway's rules, and names the reason for any refusal.
To check your session:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Gateway Authentication**.
3. Go to **Would the gateway let me through?**.
4. Enter the **Method** and the **Path, as it would arrive at the gateway**.
5. Select **Check my session**.

## Request access to a secret

Request access to a secret asks for a short-lived lease on it. A refused request never becomes a lease and appears in the access audit log instead.
To request access to a secret:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Secret Vault**.
3. Go to **Request access**.
4. Enter the **Secret path** and the **Requesting principal**.
5. Select **Request access**.

## Rotate a secret

Rotate a secret replaces the secret material without exposing the old value.
To rotate a secret:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Secret Vault**.
3. Go to **Rotate a secret**.
4. Enter the **Secret policy ID** and the **Request ID**.
5. Select **Rotate secret**.

## Features that can be switched on and off

Features can be switched on for a scope and a group of people, and every change is kept in history rather than overwritten.
To switch a feature on or off:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Feature Flags & Config**.
3. Go to **Features that can be switched on and off**.
4. Choose **Which feature**, **Where it applies** and **Who it applies to**, and the **Share of people who get it**.
5. Select **Save this setting**.

## Send a notification

Send a notification delivers a notice to one person through a chosen channel.
To send a notification:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Notifications**.
3. Go to **Send a notification**.
4. Enter the **Recipient principal id**, the **Channel** and the **Template**, then the **Subject** and **Body**.
5. Optionally add a **Recipient address override**, the **Source event type** and a **Source reference**.
6. Select **Send notification**.
The Delivery register shows each notice; FAILED rows are recorded proof that a notice did not go out.

## Define a role

Define a role creates a role that people can then be granted.
To define a role:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Identity, Scope & Foundation**, then select **Roles & Access**.
3. Go to **Define a role**.
4. Enter the **Role code**, the **Display name**, the **Scope** and the **Legal entity**.
5. Select **Define role**.
6. To give the role permissions, use **Attach a permission bundle**: choose the **Role**, enter the **Bundle code** and **Permitted actions**, and select **Attach bundle**.
