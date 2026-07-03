@'
# ZoikoSuite Domain Cells

@governance — Owns Policy, Authorization, Workflow & Approvals, Obligations,
  Governance Decision Log. Never writes business-domain code. Reviews any
  PR that touches the Governance Plane.

@identity — Owns Identity Context Service, Tenant & Entity Registry,
  Secret Vault Integration, Access Control.

@evidence — Owns Audit Event Store, Document Vault, Workflow History,
  Evidence Manifest. Read-only consumer of every other domain's events;
  never mutates source truth.

@finance — Owns Ledger, AP/AR, Treasury, Reconciliation, Close.

@workforce — Owns Employee Master, Contracts, Payroll, Benefits.

Each persona reads docs/architecture/03-microservices.md for its owned
service specs before writing code. Each persona must follow
.agents/rules/doctrine.md.
'@ | Set-Content -Path ".agents\agents.md" -Encoding utf8