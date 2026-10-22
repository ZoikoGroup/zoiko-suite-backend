package handler

import (
	"strings"
	"time"

	"zoiko.io/privacy-transfer-svc/internal/domain"
)

// triggerInput is everything the §17.1 trigger evaluation can look at. Both
// EvaluateTransfer and the trigger-evaluation endpoint use computeTriggers, so
// the two can never disagree about whether a reassessment is needed.
type triggerInput struct {
	Relationship  *domain.ProcessorRelationship
	Mechanism     *domain.TransferMechanism // nil when not supplied / not found
	Assessment    *domain.TransferAssessment
	Subprocessors []domain.Subprocessor
	Destination   string
	DataCats      []string
	SubjectCls    []string
	Declared      string
	Now           time.Time
}

// triggerOutcome is one §17.1 trigger. Derived reports whether it was
// established from stored evidence (as opposed to only declared by the
// caller). Triggers 2 (new automated decisioning), 5 (material architecture
// change) and 7 (security/privacy incident) concern facts that originate in
// other systems and are not recorded anywhere in this service, so they can
// only ever be caller-declared — a CROSS-SERVICE / EXTERNAL DEPENDENCY, not
// something this service can derive.
type triggerOutcome struct {
	domain.TriggerEvaluation
	Derived bool
}

func isMinorsClass(list []string) bool {
	for _, c := range list {
		switch strings.ToUpper(strings.TrimSpace(c)) {
		case "MINOR", "MINORS", "CHILD", "CHILDREN":
			return true
		}
	}
	return false
}

// computeTriggers evaluates all eight §17.1 triggers, in §17.1 order.
func computeTriggers(in triggerInput) []triggerOutcome {
	mk := func(t, desc, code string) triggerOutcome {
		return triggerOutcome{TriggerEvaluation: domain.TriggerEvaluation{Trigger: t, Description: desc, ReasonCode: code}}
	}
	declared := func(t string) bool { return in.Declared == t }
	rel := in.Relationship

	// 1. New purpose / sensitive category: a sensitive category the
	// relationship was not registered for (exact membership in the sensitive
	// taxonomy, not a substring match).
	t1 := mk(domain.TriggerNewPurposeOrSensitiveCategory,
		"New purpose, materially expanded purpose or new sensitive/special data category",
		"PRV-016: REASSESSMENT_TRIGGER_NEW_PURPOSE_OR_SENSITIVE_CATEGORY")
	for _, cat := range in.DataCats {
		if domain.IsSensitiveCategory(cat) && !contains(rel.DataCategories, cat) {
			t1.Triggered, t1.Derived = true, true
		}
	}
	t1.Triggered = t1.Triggered || declared(t1.Trigger)

	// 2. New automated decisioning — declared only.
	t2 := mk(domain.TriggerAutomatedDecisioning,
		"New automated decisioning/profiling or materially increased effect on individuals",
		"PRV-016: REASSESSMENT_TRIGGER_AUTOMATED_DECISIONING")
	t2.Triggered = declared(t2.Trigger)

	// 3. New processor/subprocessor or destination jurisdiction. A
	// subprocessor attached AFTER the latest assessment was never assessed.
	t3 := mk(domain.TriggerNewProcessorOrJurisdiction,
		"New processor/subprocessor or destination jurisdiction/region",
		"PRV-016: REASSESSMENT_TRIGGER_NEW_DESTINATION_JURISDICTION")
	if in.Destination != "" && len(rel.Jurisdictions) > 0 && !contains(rel.Jurisdictions, in.Destination) {
		t3.Triggered, t3.Derived = true, true
	}
	if !t3.Triggered && in.Assessment != nil {
		for _, sp := range in.Subprocessors {
			if sp.CreatedAt.After(in.Assessment.CreatedAt) {
				t3.Triggered, t3.Derived = true, true
				t3.ReasonCode = "PRV-016: REASSESSMENT_TRIGGER_NEW_SUBPROCESSOR"
				break
			}
		}
	}
	t3.Triggered = t3.Triggered || declared(t3.Trigger)

	// 4. Changed mechanism: the mechanism was recorded after the assessment.
	t4 := mk(domain.TriggerChangedTransferMechanism,
		"Changed transfer mechanism, legal validity, processing instruction or recipient category",
		"PRV-016: REASSESSMENT_TRIGGER_CHANGED_TRANSFER_MECHANISM")
	if in.Mechanism != nil && in.Assessment != nil && in.Mechanism.CreatedAt.After(in.Assessment.CreatedAt) {
		t4.Triggered, t4.Derived = true, true
	}
	t4.Triggered = t4.Triggered || declared(t4.Trigger)

	// 5. Material architecture change — declared only.
	t5 := mk(domain.TriggerMaterialArchitectureChange,
		"Material architecture change affecting data access, observability, model training or re-identification risk",
		"PRV-016: REASSESSMENT_TRIGGER_MATERIAL_ARCHITECTURE_CHANGE")
	t5.Triggered = declared(t5.Trigger)

	// 6. New minors context.
	t6 := mk(domain.TriggerNewMinorsContext,
		"New children/minors context or materially changed age-assurance model",
		"PRV-016: REASSESSMENT_TRIGGER_NEW_MINORS_CONTEXT")
	if isMinorsClass(in.SubjectCls) && !isMinorsClass(rel.SubjectClasses) {
		t6.Triggered, t6.Derived = true, true
	}
	t6.Triggered = t6.Triggered || declared(t6.Trigger)

	// 7. Security/privacy incident — declared only.
	t7 := mk(domain.TriggerSecurityPrivacyIncident,
		"Privacy/security incident revealing an unmodeled risk or control failure",
		"PRV-016: REASSESSMENT_TRIGGER_SECURITY_PRIVACY_INCIDENT")
	t7.Triggered = declared(t7.Trigger)

	// 8. Expiry / review date reached.
	t8 := mk(domain.TriggerExpiryReviewDateReached,
		"Expiry/review date reached or PDC legal-rule package marks prior authorization stale",
		"PRV-016: ASSESSMENT_EXPIRED")
	if in.Assessment != nil && in.Assessment.ExpiredAsOf(in.Now) {
		t8.Triggered, t8.Derived = true, true
	}
	t8.Triggered = t8.Triggered || declared(t8.Trigger)

	return []triggerOutcome{t1, t2, t3, t4, t5, t6, t7, t8}
}
