package events_test

// The event contract (asyncapi.yaml) and the code agree both ways: every event
// type the service can publish or consume is documented, and nothing is
// documented that the code does not have.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"zoiko.io/jurisdiction-rules-svc/internal/consumer"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

func codeEvents() []string {
	return []string{
		events.EventBusinessCalendarUpdated,
		events.EventCalculationCompleted,
		events.EventCalculationFailed,
		events.EventCalendarChanged,
		events.EventEInvoiceAccepted,
		events.EventEInvoiceRejected,
		events.EventEInvoiceStatus,
		events.EventEInvoiceSubmitted,
		handler.EventEntityApplicabilityBackfilled,
		consumer.EventEntityCreated,
		consumer.EventEntityJurisdictionChanged,
		events.EventFilingAccepted,
		events.EventFilingRejected,
		events.EventFilingStatus,
		events.EventFilingSubmitted,
		events.EventHolidayOccurrenceChanged,
		events.EventHotfixDeclared,
		events.EventHotfixRetrospective,
		events.EventInterpretationApproved,
		events.EventInterpretationRecorded,
		events.EventJurisdictionCreated,
		events.EventJurisdictionDeactivated,
		events.EventJurisdictionDecisionResolved,
		events.EventJurisdictionHierarchyChanged,
		events.EventJurisdictionSupportChange,
		events.EventJurisdictionUpdated,
		events.EventLegalDriftDetected,
		events.EventObligationDueDated,
		events.EventObligationRulePublished,
		handler.EventOutboxEventRequeued,
		events.EventPackBlocked,
		events.EventPackCertified,
		events.EventPackCreated,
		events.EventPackPromoted,
		events.EventPackReleased,
		events.EventPackReviewed,
		events.EventPackRolledBack,
		events.EventPackSuperseded,
		events.EventPackTestsRun,
		events.EventPackUnblocked,
		events.EventPackVerificationFailed,
		events.EventPackVersionCompiled,
		events.EventPackVersionDrafted,
		events.EventPackVersionSigned,
		events.EventPackVersionSubmitted,
		events.EventPackWithdrawn,
		events.EventPaymentTermActivated,
		events.EventPaymentTermRetired,
		events.EventPaymentTermSuperseded,
		events.EventRegimeCreated,
		consumer.EventRegulatorySourceChanged,
		events.EventRemediationClosed,
		events.EventRemediationOpened,
		events.EventRolloutAttested,
		events.EventRolloutCreated,
		events.EventRolloutExpertDecided,
		events.EventRolloutStatusChanged,
		events.EventRuleActivated,
		events.EventRuleEffective,
		events.EventRuleExpired,
		events.EventRuleProvenanceSet,
		events.EventRuleUpdated,
		events.EventRuleVersioned,
		events.EventSigningKeyRegistered,
		events.EventSigningKeyRetired,
		events.EventSigningKeyRevoked,
		events.EventSourceCaptured,
		events.EventSourceChangeCaptured,
		events.EventSourceChangeClosed,
		events.EventSourceChangeInReview,
		events.EventSourceReviewed,
		events.EventSourceSuperseded,
	}
}

func TestAsyncAPIDocumentsEveryEventAndOnlyThose(t *testing.T) {
	raw, err := os.ReadFile("../../asyncapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^      name: (\S+)\s*$`).FindAllStringSubmatch(strings.ReplaceAll(string(raw), "\r", ""), -1) {
		documented[m[1]] = true
	}
	inCode := map[string]bool{}
	for _, e := range codeEvents() {
		inCode[e] = true
	}
	var missing, extra []string
	for e := range inCode {
		if !documented[e] {
			missing = append(missing, e)
		}
	}
	for e := range documented {
		if !inCode[e] {
			extra = append(extra, e)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("asyncapi.yaml out of step with the code\nin code, not documented: %v\ndocumented, not in code: %v", missing, extra)
	}
	if len(inCode) < 70 {
		t.Fatalf("only %d event types collected; the contract test is not seeing the code", len(inCode))
	}
}
