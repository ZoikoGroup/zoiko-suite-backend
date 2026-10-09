// Package ncd holds the stable error and reason codes of ZS-SVC-Y-001 section 10.3.
//
// A stable code is the part of an error a caller can build on: messages are reworded, status
// codes follow HTTP conventions, but NCD-008 always means PRIVACY_PERMISSION_BLOCKED. The
// service's own API error codes (intent_not_found, quiet_hours, ...) predate this list and
// stay as they are; the stable code is added beside them, never in place of them.
//
// A code is stamped only where the service genuinely produces that condition. A code nothing
// can yet produce is listed in Unproduced with the reason, so the catalogue cannot suggest a
// control that does not exist.
package ncdcode

import "fmt"

// The stable codes, exactly as the standard spells them.
const (
	IntentNotFound            = "NCD-001"
	IntentNotEffective        = "NCD-002"
	TemplateNotPublished      = "NCD-003"
	TemplateVariableInvalid   = "NCD-004"
	LocaleNotApproved         = "NCD-005"
	RecipientUnresolved       = "NCD-006"
	EndpointUnverified        = "NCD-007"
	PrivacyPermissionBlocked  = "NCD-008"
	MarketingPermissionBlock  = "NCD-009"
	ChannelSuppressed         = "NCD-010"
	NoCompliantChannel        = "NCD-011"
	QuietHourDeferred         = "NCD-012"
	ProviderRouteUnavailable  = "NCD-013"
	DeliveryAttemptUnknown    = "NCD-014"
	DeliveryExpired           = "NCD-015"
	EvidenceInsufficient      = "NCD-016"
	AcknowledgmentInvalid     = "NCD-017"
	RegulatedNoticeReviewReq  = "NCD-018"
	DuplicateCommunication    = "NCD-019"
	CrossTenantRecipientBlock = "NCD-020"
)

// Names maps every code to its stable meaning.
var Names = map[string]string{
	IntentNotFound:            "INTENT_NOT_FOUND",
	IntentNotEffective:        "INTENT_NOT_EFFECTIVE",
	TemplateNotPublished:      "TEMPLATE_NOT_PUBLISHED",
	TemplateVariableInvalid:   "TEMPLATE_VARIABLE_INVALID",
	LocaleNotApproved:         "LOCALE_NOT_APPROVED",
	RecipientUnresolved:       "RECIPIENT_UNRESOLVED",
	EndpointUnverified:        "ENDPOINT_UNVERIFIED",
	PrivacyPermissionBlocked:  "PRIVACY_PERMISSION_BLOCKED",
	MarketingPermissionBlock:  "MARKETING_PERMISSION_BLOCKED",
	ChannelSuppressed:         "CHANNEL_SUPPRESSED",
	NoCompliantChannel:        "NO_COMPLIANT_CHANNEL",
	QuietHourDeferred:         "QUIET_HOUR_DEFERRED",
	ProviderRouteUnavailable:  "PROVIDER_ROUTE_UNAVAILABLE",
	DeliveryAttemptUnknown:    "DELIVERY_ATTEMPT_UNKNOWN",
	DeliveryExpired:           "DELIVERY_EXPIRED",
	EvidenceInsufficient:      "EVIDENCE_INSUFFICIENT",
	AcknowledgmentInvalid:     "ACKNOWLEDGMENT_INVALID",
	RegulatedNoticeReviewReq:  "REGULATED_NOTICE_REVIEW_REQUIRED",
	DuplicateCommunication:    "DUPLICATE_COMMUNICATION",
	CrossTenantRecipientBlock: "CROSS_TENANT_RECIPIENT_BLOCKED",
}

// Unproduced lists the codes this service cannot yet produce, with the reason. The test that
// guards the catalogue requires every code to be either produced or listed here. Since the
// 7 Oct merge the NCD control plane (internal/ncd) emits NCD-007, NCD-018, NCD-019 and
// NCD-020, so they are no longer listed.
var Unproduced = map[string]string{
	MarketingPermissionBlock: "marketing runs through the ledger pipeline; only the direct path's refusal of marketing classes carries this code",
}

// Format renders "NCD-008 PRIVACY_PERMISSION_BLOCKED" for a reason string. An unknown code
// panics: it is a programming error to stamp a code the catalogue does not define, and a
// reason that quietly lacked its name would defeat the point of a stable code.
func Format(code string) string {
	name, ok := Names[code]
	if !ok {
		panic(fmt.Sprintf("ncd: %q is not a stable code", code))
	}
	return code + " " + name
}

// apiCodes maps the service's own API error codes onto the stable codes. Only conditions that
// mean exactly the stable code are mapped; a generic code (store_unavailable, invalid_json)
// is never given one.
var apiCodes = map[string]string{
	"intent_not_found":                 IntentNotFound,
	"intent_version_not_found":         IntentNotFound,
	"intent_not_effective":             IntentNotEffective,
	"intent_retired":                   IntentNotEffective,
	"unknown_template":                 TemplateNotPublished,
	"template_version_not_found":       TemplateNotPublished,
	"template_retired":                 TemplateNotPublished,
	"missing_variables":                TemplateVariableInvalid,
	"missing_template_variables":       TemplateVariableInvalid,
	"unexpected_variables":             TemplateVariableInvalid,
	"unexpected_template_variables":    TemplateVariableInvalid,
	"template_variable_invalid":        TemplateVariableInvalid,
	"locale_not_approved":              LocaleNotApproved,
	"recipient_unresolved":             RecipientUnresolved,
	"no_compliant_channel":             NoCompliantChannel,
	"unsupported_channel":              ProviderRouteUnavailable,
	"DELIVERY_OUTCOME_UNKNOWN":         DeliveryAttemptUnknown,
	"evidence_insufficient":            EvidenceInsufficient,
	"operator_acknowledgement_refused": AcknowledgmentInvalid,
	"notice_already_answered":          AcknowledgmentInvalid,
	"acknowledgement_not_required":     AcknowledgmentInvalid,
	"acknowledgement_invalid":          AcknowledgmentInvalid,
}

// ForAPIError returns the stable code for one of the service's API error codes.
func ForAPIError(apiCode string) (string, bool) {
	c, ok := apiCodes[apiCode]
	return c, ok
}

// APICodes returns a copy of the mapping, for the catalogue's tests.
func APICodes() map[string]string {
	out := make(map[string]string, len(apiCodes))
	for k, v := range apiCodes {
		out[k] = v
	}
	return out
}
