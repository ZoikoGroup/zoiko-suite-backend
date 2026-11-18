package catalogue

// extensionNamingDebt lists extension event types whose last segment is not a
// past-tense fact (ZS-EVENT-001 section 5.1, E-01). They are emitted by services
// today under these names, so the catalogue records them as they are rather than
// pretend they conform: a state ("pending", "partial"), a noun ("variance",
// "cutover") or an adjective ("invalid", "duplicate") is not a fact that happened.
//
// This is debt, not approval. The list may only shrink: TestNamingDebtIsReal
// fails when an entry now conforms (rename done, delete the line) or is no
// longer catalogued. A NEW extension with such a name is refused by Lint.
var extensionNamingDebt = map[string]bool{
	"com.zoikosuite.accounting.period.subledger-control-exception":       true,
	"com.zoikosuite.integration.batch.conflict":                          true,
	"com.zoikosuite.integration.batch.duplicate":                         true,
	"com.zoikosuite.integration.batch.partial":                           true,
	"com.zoikosuite.integration.callback.duplicate":                      true,
	"com.zoikosuite.integration.callback.inconsistent":                   true,
	"com.zoikosuite.integration.callback.invalid":                        true,
	"com.zoikosuite.integration.connection.authorizing":                  true,
	"com.zoikosuite.integration.connection.credential-expiring":          true,
	"com.zoikosuite.integration.inbound.duplicate":                       true,
	"com.zoikosuite.integration.legacy.cutover":                          true,
	"com.zoikosuite.integration.outbound.pending":                        true,
	"com.zoikosuite.integration.reconciliation.variance":                 true,
	"com.zoikosuite.integration.security.batch-content-unsafe":           true,
	"com.zoikosuite.integration.security.cross-tenant-credential":        true,
	"com.zoikosuite.integration.security.inbound-account-claim-mismatch": true,
	"com.zoikosuite.integration.security.inbound-tenant-claim-mismatch":  true,
	"com.zoikosuite.integration.security.tenant-scope-mismatch":          true,
	"com.zoikosuite.tax.authority-attempt.deadline-at-risk":              true,
	"com.zoikosuite.tax.authority-attempt.unknown-outcome":               true,
}
