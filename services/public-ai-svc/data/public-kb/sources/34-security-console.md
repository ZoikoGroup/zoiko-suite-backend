---
source_id: kb-security-console
title: Security & Trust page
domain: Security & Trust
module_id: security-console
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

## Security & Trust page

The Security & Trust page governs cryptographic keys, mutual TLS, security event streaming, zero-trust access evaluation and AI safety policies. It shows mTLS certificates, the SIEM threat feed, CARTA zero-trust checks, KMS cryptographic keys and AI governance runs, with **Refresh Stream**, **Issue mTLS Cert** and **Rotate Active Key** at the top.

## Cryptographic Trust Chain & AI Governance Pipeline

The cryptographic trust chain has five steps: mTLS mutual authentication between services, threat telemetry streamed to the SIEM, CARTA zero-trust risk assessment, envelope encryption of data keys, and AI safety guardrails.

## mTLS service certificates

Mutual TLS uses X.509 certificates so every service proves who it is to every other service, with certificate authority rotation and SAN pinning.
To issue an mTLS certificate:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Security & Trust**, then select **Security & Trust**.
3. At the top of the page, select **Issue mTLS Cert**.
4. Follow the prompts for the service the certificate is for.
Issued certificates appear under X.509 Service Certificates & SAN Bindings.

## SIEM threat feed

The SIEM threat feed streams security telemetry, audit trails, authentication events and rate-limit triggers to the security information and event management system in real time. Filter it by severity and select **Refresh Stream** to update it.

## CARTA zero-trust access evaluation

CARTA (Continuous Adaptive Risk and Trust Assessment) scores every access request from the person's identity, device posture and context, and decides ALLOW, STEP_UP_MFA (ask for a second factor) or DENY, with a risk level from low to critical.
To evaluate a test subject:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Security & Trust**, then select **Security & Trust**.
3. Scroll to the **CARTA** panel.
4. Select **Evaluate Test Subject**.
5. Read the decision, trust score, device trust and reasons that appear in the list.

## Key management and envelope encryption

Key management holds master keys in a hardware security module (HSM) and uses envelope encryption: each data key is generated and wrapped by an HSM-backed root key, with bring-your-own-key support.
To rotate the active key:
1. Sign in to your ZoikoSuite account. If you do not have one yet, ask your organization's administrator for access.
2. In the left menu, open **Security & Trust**, then select **Security & Trust**.
3. At the top of the page, select **Rotate Active Key**.
4. Confirm the rotation when asked.
Key rotation is a high-risk action that needs a person in the loop.

## AI governance runs

AI governance tracks every AI run with its model, purpose, tokens, cost and guardrail outcome: passed, flagged or blocked. Its action risk registry gives each automated action a risk tier: tier 1 (critical) actions are blocked at the console gate, and lower tiers can only propose, with a person in the loop where marked.
