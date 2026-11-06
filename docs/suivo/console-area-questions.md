# Console-area questions for the ZoikoSuite Assistant

Questions about the areas of the ZoikoSuite console, with the knowledge article that answers each.
They are checked automatically by `acceptance.TestConsoleAreasAreAnswered`. The articles are drafts
(`DRAFT_PENDING_REVIEW`) written from the console's own page descriptions: they are answered in
local/dev, and need owner approval before production (Doc 03 §10, Doc 09 §16).

| Area | Try asking | Answered from |
|---|---|---|
| My Work | "use of Task", "what is Tasks & Approvals", "what are Exceptions", "what is Assigned Actions" | `kb-my-work` |
| Identity Context | "use of the Identity Context", "what is the Authentication & federation", "how does support access work" | `kb-identity-context` |
| Identity, Scope & Foundation | "what is Secret Vault", "what is Gateway Authentication", "what is Roles & Access", "what is Tenants & Entities", "what are Feature Flags & Config", "what is Notifications" | `kb-identity-foundation` |
| Delegated Authority | "what is Delegated Authority" | `kb-delegated-authority` |
| Jurisdictions & Rules | "how to Register a jurisdiction", "what is Jurisdictions & Rules", "how do I resolve a rule pack", "can I delete a jurisdiction" | `kb-jurisdictions-rules` |
| Governance Log and Policies | "what is the Governance Log", "how do policy versions work", "how to change an approval limit policy" | `kb-governance-log-policies` (or `kb-delegated-authority` for limits) |
| Obligations | "what are Obligations", "how to raise an obligation" | `kb-obligations` |
| Authorization and Workflows | "what is Authorization", "what is segregation of duties", "what is Workflows & Approvals" | `kb-authorization-workflows`, `kb-governance` or `kb-security-trust` |
| Overview | "what is the Executive Dashboard", "what is Domain Health" | `kb-console-overview` |
| Governance Log & Policies sections | "use of the Decision log", "What is in force right now", "use of the create rule", "Set the limit", "how do I bring a limit into force" | `kb-governance-log-policies` |
| Obligations sections | "how do I move an obligation's status", "what is a filing requirement" | `kb-obligations` |
| Authorization & Workflows sections | "how is a permission check decided", "what is the elevated access lifecycle", "how do I start a workflow", "how do I decide the current stage", "what are workflow definitions" | `kb-authorization-workflows` |
| Evidence Requirements | "what does NO_REQUIREMENTS_DEFINED mean", "use of the Requirement catalog", "how do I withdraw a requirement" | `kb-evidence-requirements` |
| Finance & Payments (journals) | "how do I record a journal", "how do I ingest a statement line", "what is the Reconciliation register" | `kb-finance-journals` |

Any question that names a section heading of these pages (all of its words) is treated as a product
question, so it is answered rather than given the generic "I'm here to help with…" reply.

Still refused, by design (Doc 02 X01, live data):
- "show our domain health"
- "how many services are running"
- "list my tenants"

The assistant cannot see the console's live data.

A jurisdiction question that names a country, such as "Do you support the UK jurisdiction?", is still
answered as market availability (I04).
