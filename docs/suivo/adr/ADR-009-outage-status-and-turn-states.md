# ADR-009: Outages are a 200 UNAVAILABLE reply; turn states are pipeline stages

- Status: Proposed — pending Backend Lead + Architect
- Date: 2026-11-06
- Context: Doc 04 §11 lists 503 `KNOWLEDGE_UNAVAILABLE` and 504 `MODEL_TIMEOUT` as problems, and
  Doc 04 §12 names explicit turn states (READY, POLICY_CHECK, RETRIEVE, GENERATE, VALIDATE, COMMIT).
  Doc 05 §10/§13, written later, says business outcomes are HTTP 200 and keeps problems for transport and
  protocol failures. The service already returns a typed `UNAVAILABLE` envelope with reason
  `KNOWLEDGE_UNAVAILABLE`, `MODEL_TIMEOUT` or `MODEL_UNAVAILABLE`, and the gap list (B3.5) asked for this
  divergence to be decided rather than left implicit.
- Decision:
  1. **A failed dependency during a turn is a 200 `UNAVAILABLE` envelope**, not a 503/504 problem. The
     visitor still gets approved fallback wording, official routes and a reference id, and the turn is
     committed (so its idempotency key replays the same reply and the session slot is released). The
     typed `reason` carries what Doc 04 §11 calls the error code, and `safety_disposition` is `RETRY`.
     503 `service_unavailable` stays for failures before a turn exists (session store down, not ready)
     and 504 `timeout` for the gateway path; both remain in the problem catalogue.
  2. **Turn states are the pipeline stages, not a stored state machine.** A turn is one synchronous
     request: its only persisted states are `IN_PROGRESS` and `COMPLETED`/aborted (the turn lease).
     The Doc 04 §12 states are observable as stages: `public_ai_stage_duration_seconds{stage}` records
     policy, retrieval, generation and validation, and the decision record names the outcome reason
     and the stage a reason comes from (`contract.StageFor`). Storing intermediate states would add
     writes per turn without a consumer.
- Consequences: Clients handle one envelope shape for every outcome; monitors alert on
  `public_ai_turns_total{status="UNAVAILABLE"}` (error-budget burn) rather than on 5xx. If an
  asynchronous or streaming turn is introduced (SSE, D05-07), revisit point 2: a resumable turn needs
  persisted states.
