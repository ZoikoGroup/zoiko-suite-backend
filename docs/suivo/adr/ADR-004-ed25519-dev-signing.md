# ADR-004: Ed25519 dev signing

- Status: Proposed — pending <role>
- Date: 2026-10-27
- Context: Signing/revocation owner not assigned (DEC-09). Need to enforce signed releases.
- Decision: Use Ed25519 for KB/claim release manifests in dev. Private key from env PUBLIC_AI_SIGNING_KEY (generated locally, never committed). Verify on load; unsigned/invalid → not ready. Production signing process requires DEC-09.
- Consequences: Enforces signing discipline now; keys local-only.
- Dev defaults: Local Ed25519 keypair; D08-03 pending owner approval.
