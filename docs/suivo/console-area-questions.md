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

| Authorization checks | "what is Maker and checker", "where can someone act", "what is Effective access", "what is Borrowed authority" | `kb-authorization-checks` |
| Commercial Ops | "how do I screen a counterparty", "use of the Amendment ledger", "how do I check a spend before committing it" | `kb-procurement-console` |
| Delegations | "how do I grant a delegation", "what is the Delegation register" | `kb-delegations-console` (or `kb-delegated-authority`) |
| Gateway, Secrets, Settings, Notifications, Roles | "what each refusal means", "how does secret access actually work", "what is the Delivery register", "how do I retire a role" | `kb-access-settings-console` |
| Contracts | "how do I draft a contract", "what is Version history" | `kb-contracts-console` |
| Privacy | "how do I withdraw consent", "what is a transfer mechanism", "what is a transfer risk assessment" | `kb-privacy-transfers` |
| Search | "what is Restriction propagation", "how do I build a generation", "how do I request an export" | `kb-search-admin` |
| Source Authority | "how do I resolve a value", "what is the Precedence register" | `kb-source-authority` |
| Tenants & Entities | "how do I provision a tenant", "what is Registry integrity", "what are Workspaces" | `kb-tenants-entities` |

**Every section of every article is answered.** `acceptance.TestEveryKBHeadingIsAnswered` asks
"what is <heading>" and "tell me about <heading>" for every `##` heading in the public KB (over 400
questions). A question that names a section, or whose words are mostly in one section including its
heading, is treated as a product question rather than given the generic "I'm here to help with…" reply.
A "how do I approve an invoice" style question is answered as a how-to; "approve my invoice" is still
refused (X02). `acceptance.TestOffTopicStillRefused` keeps unrelated questions out of scope.

Deliberately not answered by heading (see `headingExceptions`): "Sign-in security" (routed to account
support), "Create a data residency policy" (approved claims only), "Capabilities, not certifications"
(claims path), and two headings too generic to name an article. Console sections that are test
harnesses or placeholders (QA test matrices, "Rest of the domain") have no article, and live status
("Domain Operational Status") is refused as live data.

Still refused, by design (Doc 02 X01, live data):
- "show our domain health"
- "how many services are running"
- "list my tenants"

The assistant cannot see the console's live data.

A jurisdiction question that names a country, such as "Do you support the UK jurisdiction?", is still
answered as market availability (I04).

## Platform question bank

`services/public-ai-svc/testdata/platform-questions.tsv` holds visitor questions area by area: Overview,
Tax, Governance, Finance, Legal, Commercial Ops, Purchase Requests, Compliance, Evidence, Delegated
Authority, Reporting, Security, Integrations, Workforce, Multi-entity, Migration, Jurisdictions,
Authorization, Workflows, My Work, Identity, Privacy, Search, Source Authority and Tenants. Each
line names the articles that may answer it. `acceptance.TestPlatformQuestionBank` asks every one
and fails if a question gets the off-topic reply, is answered from an unrelated article, or shows
a heading marker. To cover a new question, add a line to the file.
