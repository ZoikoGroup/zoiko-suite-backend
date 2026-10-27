# ADR-003: Fake model provider until DEC-05

- Status: Proposed — pending <role>
- Date: 2026-10-27
- Context: LLM/vendor/region/DPA not selected (DEC-05). Must not invent vendor.
- Decision: Implement model.Provider interface with fake provider (deterministic, uses evidence only). No browsing/tools. Real provider only after DEC-05 with DPA/region approval.
- Consequences: Safe default, testable, no external calls.
- Dev defaults: PUBLIC_AI_MODEL_PROFILE=fake-v0.
