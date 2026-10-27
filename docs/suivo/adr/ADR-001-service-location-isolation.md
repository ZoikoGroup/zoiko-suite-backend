# ADR-001: Service location & isolation

- Status: Proposed — pending Global CTO + Security
- Date: 2026-10-27
- Context: R0 must be isolated (INV-01/SEC-P0-01). Repo has assistant-rag-svc; must not reuse its DB/index/topics/creds/runtime.
- Decision: Create services/public-ai-svc (module zoiko.io/public-ai-svc). Internal packages in same service (Doc 04 §05/Doc 05 §16). Enforce isolation: go list -deps must not include other zoiko.io/* modules except zoiko.io/public-ai-svc; config must not reference other services' DSN/topics. Add isolation test.
- Consequences: Single deployable unit, strict boundary enforced by test/CI.
- Dev defaults: None.
