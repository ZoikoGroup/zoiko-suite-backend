---
source_id: kb-security-trust
title: Security and Trust
domain: Security and Trust
module_id: security-trust
audience: PUBLIC_ANONYMOUS
status: DRAFT_PENDING_REVIEW
owner: owner-product
effective_from: 2026-01-01
expires_at: 2027-12-31
locale: en-US
intent_ids: I12,I13,I15
---

## Security and Trust

Security and Trust covers capabilities such as mTLS certificates, SIEM integrations, key lifecycle management and cap tables. This describes capabilities only. Certification status is published only through the official trust information and is never stated by this assistant without a verified claim.

## Service identity and keys

Services authenticate to each other with mTLS certificates that can be issued, rotated and revoked. Cryptographic keys are managed through their lifecycle, including rotation and disabling. Sensitive credentials are brokered through a secret vault rather than stored in configuration or source code.

## Access and authorization

Access is governed through roles, permission bundles and attribute-based rules, including segregation of duties checks. Material actions are designed to require an authorization decision before they run, and each decision is recorded with the actor, action, basis and outcome.

## Event monitoring and cap tables

Events such as certificate changes and failed authentication attempts can be forwarded to a connected SIEM. Cap table records are also governed in this area.

## Capabilities, not certifications

Commitments for a particular deployment come from current approved trust information or a scoped security review, not from this description.
