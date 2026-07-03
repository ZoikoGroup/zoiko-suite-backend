# .agents/rules/model-policy.md
- Governance Plane services (Identity, Policy, Authorization, Workflow,
  Audit, Decision Log): Claude Opus/Sonnet only.
- Intelligence Plane services (forecasting, anomaly, risk): Gemini 3.1 Pro (High).
- Domain service scaffolding: Gemini 3.1 Pro (High) for first draft;
  require a Claude review pass before merge on anything touching
  money, tax, or compliance logic.