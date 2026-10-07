package domain

// Errors for the link between a ledger message intent and a direct-path
// notification (ZS-SVC-Y-001 INV-02; migration 000016).
const (
	// ErrLinkTargetNotFound: the intent or the notification does not exist for
	// this tenant. Cross-tenant ids look the same on purpose.
	ErrLinkTargetNotFound = errorString("message intent or notification not found for this tenant")

	// ErrAlreadyLinked: one of the two is already linked to something else. A link
	// is one-to-one and is never silently repointed.
	ErrAlreadyLinked = errorString("message intent or notification is already linked to a different counterpart")
)

// ErrResendLedgerOwned: a notification produced by the ledger pipeline cannot be
// resent through the direct-path resend. The register row stores the rendered body
// but not the stream's sender identity or headers, so a direct resend would go out
// from the wrong sender, which for a security or marketing stream is not a detail.
const ErrResendLedgerOwned = errorString("this communication was produced by the ledger pipeline and cannot be resent through the direct path")

// DirectPathClasses are the communication classes a direct send may state. Marketing
// (M1) and lifecycle (L1) are deliberately absent: they need their own stream
// identity and one-click unsubscribe (RFC 8058), which only the ledger pipeline
// provides, and a direct send must not be a way around them (INV-07).
var DirectPathClasses = []string{"S0", "T0", "A1"}

// ValidDirectPathClass reports whether c is a class a direct send may state.
func ValidDirectPathClass(c string) bool {
	for _, k := range DirectPathClasses {
		if k == c {
			return true
		}
	}
	return false
}
