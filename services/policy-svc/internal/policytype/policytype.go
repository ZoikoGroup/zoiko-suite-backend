// Package policytype is the catalogue of policy families policy-svc knows and
// who decides each one at runtime (ZS-GOV-POL-001 §1, §3; ZS-GOV-INT-002 §1).
//
// policy-svc is the authoritative registry for every family: versions,
// effective dates, approvals and provenance. It is the runtime evaluator for
// only two of them. Spend limits are evaluated by Spend Controls, segregation
// of duties by Authorization, and notification lawfulness by Privacy with
// Notifications enforcing at dispatch. For those families a version here is a
// governed reference to the owner's published rule set, and an evaluation
// request answers NOT_IMPLEMENTED with the owner named: it never falls back to
// approval thresholds and never permits (Doc 01 §12, PD-01, PD-03, PD-04).
package policytype

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Families.
const (
	ApprovalThreshold = "APPROVAL_THRESHOLD"
	SignatoryMatrix   = "SIGNATORY_MATRIX"
	SpendControl      = "SPEND_CONTROL"
	SoDRule           = "SOD_RULE"
	LegalBasis        = "LEGAL_BASIS"
	AccountingPolicy  = "ACCOUNTING_POLICY"
)

// Owning services.
const (
	OwnerPolicy        = "policy-svc"
	OwnerSpendControls = "spend-controls-svc"
	OwnerAuthorization = "authorization-svc"
	OwnerPrivacy       = "privacy-decision-svc"
)

// Evaluation modes.
const (
	// ModeEvaluatedHere: policy-svc evaluates the family (POST /v1/policies/evaluate).
	ModeEvaluatedHere = "EVALUATED_HERE"
	// ModeResolvedHere: policy-svc resolves the family on its own route.
	ModeResolvedHere = "RESOLVED_HERE"
	// ModeReferenceOnly: policy-svc keeps a governed reference and version
	// provenance; the owning service decides at runtime.
	ModeReferenceOnly = "REFERENCE_ONLY"
)

// Info describes one family.
type Info struct {
	PolicyType    string   `json:"policy_type"`
	OwningService string   `json:"owning_service"`
	Mode          string   `json:"evaluation_mode"`
	DecisionRoute string   `json:"decision_route"`
	EnforcedBy    []string `json:"enforced_by"`
	// EvaluableHere is true only for the families whose runtime decision
	// policy-svc makes (Doc 01 §9: "Only Signatory Matrix and pre-existing
	// approval thresholds are directly evaluable here").
	EvaluableHere bool   `json:"evaluable_here"`
	Description   string `json:"description"`
	Spec          string `json:"spec"`
}

var catalogue = map[string]Info{
	ApprovalThreshold: {
		PolicyType: ApprovalThreshold, OwningService: OwnerPolicy, Mode: ModeEvaluatedHere,
		DecisionRoute: "POST /v1/policies/evaluate", EnforcedBy: []string{"calling domain service", "workflow-svc"},
		EvaluableHere: true,
		Description:   "Amount above which an action needs human approval. Existing contract, unchanged.",
		Spec:          "Doc 03 §8.1; ZS-GOV-POL-001 §1",
	},
	SignatoryMatrix: {
		PolicyType: SignatoryMatrix, OwningService: OwnerPolicy, Mode: ModeResolvedHere,
		DecisionRoute: "POST /internal/v1/policy/signatory-matrix/resolve",
		EnforcedBy:    []string{"authorization-svc (signer eligibility)", "workflow-svc (distinct-signer quorum)", "document/e-signature owner (hash-bound signature)", "payment owner (release mandate)"},
		EvaluableHere: true,
		Description:   "Distinct human signers, corporate offices, sequencing and legal review by instrument and amount band.",
		Spec:          "ZS-GOV-POL-001 §5; ZS-GOV-INT-002 §4",
	},
	SpendControl: {
		PolicyType: SpendControl, OwningService: OwnerSpendControls, Mode: ModeReferenceOnly,
		DecisionRoute: "POST /v1/spend-checks (spend-controls-svc)",
		EnforcedBy:    []string{"spend-controls-svc (limits, exposure, reservations)"},
		Description:   "Reference to a published spend rule set. Limits, exposure, FX and reservations are evaluated by Spend Controls.",
		Spec:          "ZS-GOV-POL-001 §4; ZS-GOV-INT-002 §6",
	},
	SoDRule: {
		PolicyType: SoDRule, OwningService: OwnerAuthorization, Mode: ModeReferenceOnly,
		DecisionRoute: "POST /v1/sod/evaluate (authorization-svc)",
		EnforcedBy:    []string{"authorization-svc (static and dynamic segregation of duties)"},
		Description:   "Reference to the published conflict-set version. Static and dynamic SoD are evaluated by Authorization only.",
		Spec:          "ZS-GOV-POL-001 §6; ZS-GOV-INT-002 §5",
	},
	LegalBasis: {
		PolicyType: LegalBasis, OwningService: OwnerPrivacy, Mode: ModeReferenceOnly,
		DecisionRoute: "POST /v1/privacy/decisions (privacy-decision-svc)",
		EnforcedBy:    []string{"privacy-decision-svc (purpose, lawful basis, channel permission)", "notification-svc (pre-send check)"},
		Description:   "Jurisdictional rule reference a regulated notice cites. Lawfulness is decided by Privacy; Notifications rechecks at dispatch.",
		Spec:          "ZS-GOV-POL-001 §7; ZS-GOV-INT-002 §7",
	},
	AccountingPolicy: {
		PolicyType: AccountingPolicy, OwningService: OwnerPolicy, Mode: ModeResolvedHere,
		DecisionRoute: "GET /v1/accounting-policies/resolve",
		EnforcedBy:    []string{"accounting services (posting rules)"},
		EvaluableHere: false,
		Description:   "REF-07 accounting policy: resolved, not evaluated.",
		Spec:          "REF-07",
	},
}

// Lookup returns a family's catalogue entry.
func Lookup(policyType string) (Info, bool) {
	i, ok := catalogue[policyType]
	return i, ok
}

// All lists the catalogue in a stable order.
func All() []Info {
	out := make([]Info, 0, len(catalogue))
	for _, i := range catalogue {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].PolicyType < out[b].PolicyType })
	return out
}

// ReferenceOnly reports whether another service decides the family at runtime.
func ReferenceOnly(policyType string) bool {
	i, ok := catalogue[policyType]
	return ok && i.Mode == ModeReferenceOnly
}

// ErrInvalidReference wraps every reason a reference payload is refused.
var ErrInvalidReference = errors.New("invalid policy reference")

var (
	refText      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@ ()\-]{0,255}$`)
	jurisdiction = regexp.MustCompile(`^[A-Z]{2}(-[A-Z0-9]{1,3})?$|^EU$`)
	// evaluationKeys look like local rule content. A reference family carries
	// none of them, so it can never be mistaken for, or fall back to, an
	// evaluable rule (no threshold substitution: Doc 02 §3).
	evaluationKeys = []string{"threshold_amount", "limit_amount", "limit", "effect", "bands", "allow", "permit", "decision", "outcome"}
)

// Reference is the payload of a reference-only family version.
type Reference struct {
	OwningService  string `json:"owning_service,omitempty"`
	RuleSetRef     string `json:"rule_set_ref,omitempty"`
	RuleSetVersion string `json:"rule_set_version,omitempty"`
	Jurisdiction   string `json:"jurisdiction,omitempty"`
	RuleReference  string `json:"rule_reference,omitempty"`
}

// ValidateReference checks a reference family's payload: it names the
// owner's published rule set (and, for LEGAL_BASIS, the jurisdiction and the
// citation), never rule content of its own.
func ValidateReference(policyType string, payload []byte) error {
	info, ok := catalogue[policyType]
	if !ok || info.Mode != ModeReferenceOnly {
		return nil
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidReference, fmt.Sprintf(format, args...))
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return fail("rule_payload is required: a %s version references %s's published rule set", policyType, info.OwningService)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return fail("rule_payload must be a JSON object")
	}
	for _, k := range evaluationKeys {
		if _, present := raw[k]; present {
			return fail("%s is rule content; %s is decided by %s, so its version carries a reference only", k, policyType, info.OwningService)
		}
	}
	var ref Reference
	if err := json.Unmarshal(payload, &ref); err != nil {
		return fail("%v", err)
	}
	if ref.OwningService != "" && ref.OwningService != info.OwningService {
		return fail("owning_service is %s, not %s", info.OwningService, ref.OwningService)
	}
	text := func(field, v string, required bool) error {
		switch {
		case v == "" && required:
			return fail("%s is required", field)
		case v != "" && !refText.MatchString(strings.TrimSpace(v)):
			return fail("%s is not a valid reference", field)
		}
		return nil
	}
	if policyType == LegalBasis {
		if !jurisdiction.MatchString(ref.Jurisdiction) {
			return fail("jurisdiction must be an ISO 3166 code (GB, US-CA) or EU: a legal basis is never global")
		}
		return text("rule_reference", ref.RuleReference, true)
	}
	if err := text("rule_set_ref", ref.RuleSetRef, true); err != nil {
		return err
	}
	if err := text("rule_set_version", ref.RuleSetVersion, true); err != nil {
		return err
	}
	if ref.Jurisdiction != "" && !jurisdiction.MatchString(ref.Jurisdiction) {
		return fail("jurisdiction must be an ISO 3166 code or EU")
	}
	return nil
}
