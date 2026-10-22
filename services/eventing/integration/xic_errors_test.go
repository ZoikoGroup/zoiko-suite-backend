package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The 18 codes are a contract (ZS-SVC-T-001 §14.2): this list is the spec's, and the test fails if one is
// renumbered, dropped, or added without the spec changing.
var specXIC = map[XICCode]string{
	"XIC-001": "CONNECTION_NOT_ACTIVE", "XIC-002": "CONTRACT_VERSION_MISMATCH", "XIC-003": "SCHEMA_VERSION_UNSUPPORTED",
	"XIC-004": "AUTHENTICATION_FAILED", "XIC-005": "REPLAY_WINDOW_FAILED", "XIC-006": "DUPLICATE_EXCHANGE",
	"XIC-007": "MAPPING_FAILED", "XIC-008": "TENANT_SCOPE_MISMATCH", "XIC-009": "EGRESS_TARGET_BLOCKED",
	"XIC-010": "OUTBOUND_UNKNOWN", "XIC-011": "CALLBACK_INVALID", "XIC-012": "RATE_LIMITED",
	"XIC-013": "BATCH_INTEGRITY_FAILED", "XIC-014": "BATCH_PARTIAL", "XIC-015": "REPLAY_APPROVAL_REQUIRED",
	"XIC-016": "RESIDENCY_ROUTE_BLOCKED", "XIC-017": "CERTIFICATION_STALE", "XIC-018": "RECONCILIATION_VARIANCE",
}

func TestXICTaxonomyMatchesSpec(t *testing.T) {
	got := XICCodes()
	if len(got) != len(specXIC) {
		t.Fatalf("defined %d codes, spec has %d", len(got), len(specXIC))
	}
	for _, c := range got {
		name, ok := specXIC[c]
		if !ok {
			t.Errorf("%s is not in the spec", c)
			continue
		}
		if c.Info().Name != name {
			t.Errorf("%s is %q, spec says %q", c, c.Info().Name, name)
		}
		if !c.Valid() {
			t.Errorf("%s not valid", c)
		}
	}
}

func TestXICInfoIsCompleteAndSane(t *testing.T) {
	for _, c := range XICCodes() {
		i := c.Info()
		if i.Title == "" || i.Behavior == "" || i.Status < 200 || i.Status > 599 {
			t.Errorf("%s has an incomplete definition: %+v", c, i)
		}
	}
	// ambiguous outcomes are never retryable: the original attempt is resolved first (I-14)
	if XICOutboundUnknown.Info().Retryable {
		t.Error("an UNKNOWN outbound outcome must not be retryable")
	}
	// UNKNOWN is accepted, not failed
	if XICOutboundUnknown.Info().Status != http.StatusAccepted {
		t.Error("UNKNOWN must not read as a failure")
	}
	// rate limits are the only retryable-as-is condition
	for _, c := range XICCodes() {
		if c.Info().Retryable && c != XICRateLimited {
			t.Errorf("%s must not be retryable as-is", c)
		}
	}
	// security-relevant refusals raise telemetry (§14.2)
	for _, c := range []XICCode{XICAuthenticationFailed, XICTenantScopeMismatch, XICEgressTargetBlocked, XICReplayWindowFailed, XICCallbackInvalid} {
		if !c.Info().SecuritySignal {
			t.Errorf("%s must raise a security signal", c)
		}
	}
	// held subjects: schema, mapping, contract mismatch, batch integrity
	for _, c := range []XICCode{XICSchemaVersionUnsupported, XICMappingFailed, XICContractVersionMismatch, XICBatchIntegrityFailed} {
		if !c.Info().Quarantine {
			t.Errorf("%s holds its subject and must say so", c)
		}
	}
}

func TestXICUndefinedCodeIsAServerError(t *testing.T) {
	if XICCode("XIC-999").Valid() {
		t.Fatal("unknown code is valid")
	}
	if XICCode("XIC-999").Info().Status != http.StatusInternalServerError {
		t.Fatal("a typo must not masquerade as a client error")
	}
}

func TestXICErrorChain(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.5:443: refused")
	e := NewXIC(XICEgressTargetBlocked, "destination is not permitted").Wrap(cause)
	if !errors.Is(e, cause) {
		t.Fatal("cause not reachable")
	}
	wrapped := fmt.Errorf("while sending: %w", e)
	got, ok := AsXIC(wrapped)
	if !ok || got.Code != XICEgressTargetBlocked {
		t.Fatalf("AsXIC = %v %v", got, ok)
	}
	if _, ok := AsXIC(errors.New("plain")); ok {
		t.Fatal("plain error is not an XIC error")
	}
	if !strings.Contains(e.Error(), "XIC-009") {
		t.Fatalf("error text %q", e.Error())
	}
}

func TestXICProblemEnvelope(t *testing.T) {
	e := NewXIC(XICConnectionNotActive, "connection is suspended").WithConnection("conn-1").
		WithRemediation("reauthorize the connection")
	p := XICProblem(e, "trace-9")
	raw, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"type", "title", "status", "code", "trace_id", "connection_id", "remediation"} {
		if _, ok := m[k]; !ok {
			t.Errorf("problem lacks %q", k)
		}
	}
	if m["type"] != ProblemTypePrefix+"XIC-001" || m["status"].(float64) != 409 {
		t.Fatalf("problem = %v", m)
	}
	// only the §14.3 members: the cause and anything internal never appear
	allowed := map[string]bool{"type": true, "title": true, "status": true, "code": true, "name": true, "detail": true,
		"retryable": true, "trace_id": true, "connection_id": true, "remediation": true}
	for k := range m {
		if !allowed[k] {
			t.Errorf("problem leaks member %q", k)
		}
	}
	// no connection id, no trace id => the members are absent, not empty
	bare := XICProblem(NewXIC(XICRateLimited, ""), "")
	if _, ok := bare["connection_id"]; ok {
		t.Error("connection_id must be omitted when not disclosed")
	}
	if _, ok := bare["trace_id"]; ok {
		t.Error("trace_id must be omitted when empty")
	}
}

func TestXICProblemNeverEchoesTheCause(t *testing.T) {
	e := NewXIC(XICAuthenticationFailed, "credential rejected").Wrap(errors.New("secret=hunter2 host=db.internal"))
	raw, _ := json.Marshal(XICProblem(e, "t"))
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "db.internal") {
		t.Fatalf("cause leaked into the problem body: %s", raw)
	}
}
