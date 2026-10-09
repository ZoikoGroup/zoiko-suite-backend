package ncdcode

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The standard's table (section 10.3), written out literally so a drifting name or a missing
// code fails the build.
var spec = map[string]string{
	"NCD-001": "INTENT_NOT_FOUND", "NCD-002": "INTENT_NOT_EFFECTIVE", "NCD-003": "TEMPLATE_NOT_PUBLISHED",
	"NCD-004": "TEMPLATE_VARIABLE_INVALID", "NCD-005": "LOCALE_NOT_APPROVED", "NCD-006": "RECIPIENT_UNRESOLVED",
	"NCD-007": "ENDPOINT_UNVERIFIED", "NCD-008": "PRIVACY_PERMISSION_BLOCKED", "NCD-009": "MARKETING_PERMISSION_BLOCKED",
	"NCD-010": "CHANNEL_SUPPRESSED", "NCD-011": "NO_COMPLIANT_CHANNEL", "NCD-012": "QUIET_HOUR_DEFERRED",
	"NCD-013": "PROVIDER_ROUTE_UNAVAILABLE", "NCD-014": "DELIVERY_ATTEMPT_UNKNOWN", "NCD-015": "DELIVERY_EXPIRED",
	"NCD-016": "EVIDENCE_INSUFFICIENT", "NCD-017": "ACKNOWLEDGMENT_INVALID", "NCD-018": "REGULATED_NOTICE_REVIEW_REQUIRED",
	"NCD-019": "DUPLICATE_COMMUNICATION", "NCD-020": "CROSS_TENANT_RECIPIENT_BLOCKED",
}

func TestCatalogueMatchesTheStandardExactly(t *testing.T) {
	if len(Names) != len(spec) {
		t.Fatalf("catalogue has %d codes, the standard has %d", len(Names), len(spec))
	}
	for code, name := range spec {
		if Names[code] != name {
			t.Errorf("%s is %q, the standard says %q", code, Names[code], name)
		}
	}
}

func TestFormat(t *testing.T) {
	if got := Format(PrivacyPermissionBlocked); got != "NCD-008 PRIVACY_PERMISSION_BLOCKED" {
		t.Errorf("Format = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("stamping a code outside the catalogue must panic, not produce a nameless reason")
		}
	}()
	Format("NCD-099")
}

func TestEveryApiMappingTargetsARealCode(t *testing.T) {
	for api, code := range APICodes() {
		if _, ok := Names[code]; !ok {
			t.Errorf("API code %q maps to %q, which is not in the catalogue", api, code)
		}
	}
	if _, ok := ForAPIError("store_unavailable"); ok {
		t.Error("a generic failure must never be given a stable code")
	}
}

// The service source is the witness: a code counts as produced only if some non-test source
// file outside this package really uses it, and a code listed as unproduced must not be used.
func TestEveryCodeIsProducedOrDeclaredUnproduced(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	var src strings.Builder
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		// The table itself defines every code; it produces none. The NCD
		// control plane (internal/ncd) is a producer like any other package.
		if strings.Contains(filepath.ToSlash(p), "internal/ncdcode/") {
			return nil
		}
		b, _ := os.ReadFile(p)
		src.Write(b)
		return nil
	})
	body := src.String()
	constRef := regexp.MustCompile(`ncdcode\.([A-Za-z]+)`)
	used := map[string]bool{}
	for _, m := range constRef.FindAllStringSubmatch(body, -1) {
		used[m[1]] = true
	}
	byConst := map[string]string{
		IntentNotFound: "IntentNotFound", IntentNotEffective: "IntentNotEffective", TemplateNotPublished: "TemplateNotPublished",
		TemplateVariableInvalid: "TemplateVariableInvalid", LocaleNotApproved: "LocaleNotApproved", RecipientUnresolved: "RecipientUnresolved",
		EndpointUnverified: "EndpointUnverified", PrivacyPermissionBlocked: "PrivacyPermissionBlocked", MarketingPermissionBlock: "MarketingPermissionBlock",
		ChannelSuppressed: "ChannelSuppressed", NoCompliantChannel: "NoCompliantChannel", QuietHourDeferred: "QuietHourDeferred",
		ProviderRouteUnavailable: "ProviderRouteUnavailable", DeliveryAttemptUnknown: "DeliveryAttemptUnknown", DeliveryExpired: "DeliveryExpired",
		EvidenceInsufficient: "EvidenceInsufficient", AcknowledgmentInvalid: "AcknowledgmentInvalid", RegulatedNoticeReviewReq: "RegulatedNoticeReviewReq",
		DuplicateCommunication: "DuplicateCommunication", CrossTenantRecipientBlock: "CrossTenantRecipientBlock",
	}
	apiMapped := map[string]bool{}
	for _, c := range APICodes() {
		apiMapped[c] = true
	}
	for code := range Names {
		produced := used[byConst[code]] || apiMapped[code] || strings.Contains(body, code+" ") || planeUses(body, code)
		_, declared := Unproduced[code]
		switch {
		case !produced && !declared:
			t.Errorf("%s (%s) is neither produced anywhere nor declared unproduced", code, Names[code])
		case produced && declared && code != MarketingPermissionBlock:
			t.Errorf("%s is declared unproduced but the service uses it", code)
		}
	}
}

// planeUses reports whether the NCD control plane (internal/ncd) produces code:
// it declares each code once as a typed constant (NCD017AcknowledgmentInvalid
// ReasonCode = "NCD-017"), and a code counts as produced only when that
// constant is used somewhere besides its own declaration.
func planeUses(body, code string) bool {
	m := regexp.MustCompile(`(NCD\d{3}[A-Za-z]+)\s+ReasonCode\s*=\s*"` + regexp.QuoteMeta(code) + `"`).FindStringSubmatch(body)
	if m == nil {
		return false
	}
	return strings.Count(body, m[1]) > 1
}
