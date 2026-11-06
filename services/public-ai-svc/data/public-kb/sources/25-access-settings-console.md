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
intent_ids: I02
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

Secret access takes four steps: register the path, which grants nothing on its own; add a version saying which workloads may use it and for how long a lease may live; activate the version, until which the secret is invisible; and seed the material into the vault. An unconfigured secret is an inaccessible one, and the platform refuses rather than guessing.

## Version history and issued leases

Version history shows every version of one secret policy, which is the fastest way to see whether a version was created but never activated. Issued leases lists the grants only; a refused request never becomes a lease and appears in the access audit log instead. Provision a secret and Rotate a secret add new material without exposing the old.

## Settings

Settings brings together your current session, your identity and the platform configuration: features that can be switched on and off, and the named values services read at runtime.

## Settings services read at runtime

Settings services read at runtime are named values the rest of the platform reads to decide how to behave, such as a cutoff hour, a limit or an address. Features that can be switched on and off use the same scoping and history: nothing is overwritten, and every value is shown in plain words.

## Check exactly what a service would see

Check exactly what a service would see answers one precise question: for this name, this environment and this group, what is in force? Finding nothing means the setting is unset for that combination only, not everywhere.

## Delivery register

The Delivery register lists a legal entity's notifications, newest first. Each row records the address it was actually delivered to and where that address came from. RETRYING rows have not failed: another attempt is scheduled. FAILED rows are the ones worth acting on, because they are recorded proof that the notice did not go out. Send a notification sends one to a person through a chosen channel.

## Retire, reactivate, or rename

In Roles & Access Control, retiring a role is a real control, not a label: a retired role grants nothing on the next permission check. It is also reversible, because assignments are left intact, so reactivating a role restores them. Define a role, attach a permission bundle, and see who holds a role from the same page.
