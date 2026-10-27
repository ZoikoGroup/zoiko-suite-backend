# ADR-005: Resolved doc conflicts (dev defaults)

- Status: Proposed — pending <role>
- Date: 2026-10-27
- Context: Conflicts between docs (idempotency header vs client_turn_id, market_code malformed vs unsupported). Must use dev defaults and record.
- Decision: Apply defaults from IMPLEMENTATION_PROMPT.md:
- Idempotency: POST /sessions requires Idempotency-Key (dev default per §3); POST /turns uses Idempotency-Key = client_turn_id; header≠client_turn_id → 400 publicai.idempotency_header_mismatch.
- market_code malformed pattern → 422 validation_failed; valid but unsupported market → 200 CLARIFY.
- Assistant display name default "ZoikoSuite assistant" until brand clearance DEC-04.
- Route fixtures use https://example.invalid/... until DEC-10.
- TTL/budget/rate limits are dev defaults pending DEC-07/DEC-08/D06-05.
- Consequences: Deterministic behavior now; must be revisited on approval.
- Dev defaults: As listed above.
