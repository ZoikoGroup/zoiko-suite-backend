package envelope_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/workflow-svc/internal/envelope"
)

func TestParse_AllHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("X-Tenant-Id", "t-1")
	req.Header.Set("X-Principal-Id", "p-1")
	req.Header.Set("X-Correlation-ID", "corr-1")
	req.Header.Set("X-Request-Id", "req-1")
	req.Header.Set("X-Source-Channel", "web")
	req.Header.Set("X-Legal-Entity-Id", "le-1")
	req.Header.Set("X-Workload-Id", "wl-1")
	req.Header.Set("X-Book-Id", "b-1")
	req.Header.Set("X-Reporting-Basis", "GAAP")
	req.Header.Set("Idempotency-Key", "idem-1")
	req.Header.Set("X-Causation-Id", "cause-1")
	req.Header.Set("X-Source-System", "ext-system")
	req.Header.Set("X-External-Reference", "ext-ref-1")
	req.Header.Set("X-Occurred-At", "2026-01-15T10:30:00Z")
	req.Header.Set("X-Effective-At", "2026-01-15T11:00:00Z")
	req.Header.Set("X-Timezone", "UTC")
	req.Header.Set("X-Jurisdiction-Context", "US-CA")
	req.Header.Set("X-Purpose-Context", "audit")
	req.Header.Set("X-Expected-Version", "5")
	req.Header.Set("X-Workflow-Instance-Id", "wf-1")
	req.Header.Set("X-Approval-Reference", "appr-1")
	req.Header.Set("X-Evidence-Refs", "ev-1, ev-2, ev-3")

	env := envelope.Parse(req)

	if env.TenantID != "t-1" {
		t.Errorf("TenantID = %q, want %q", env.TenantID, "t-1")
	}
	if env.ActorSubjectID != "p-1" {
		t.Errorf("ActorSubjectID = %q, want %q", env.ActorSubjectID, "p-1")
	}
	if env.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID = %q, want %q", env.CorrelationID, "corr-1")
	}
	if env.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want %q", env.RequestID, "req-1")
	}
	if env.SourceChannel != envelope.ChannelWeb {
		t.Errorf("SourceChannel = %q, want %q", env.SourceChannel, envelope.ChannelWeb)
	}
	if env.LegalEntityID != "le-1" {
		t.Errorf("LegalEntityID = %q, want %q", env.LegalEntityID, "le-1")
	}
	if env.WorkloadID != "wl-1" {
		t.Errorf("WorkloadID = %q, want %q", env.WorkloadID, "wl-1")
	}
	if env.BookID != "b-1" {
		t.Errorf("BookID = %q, want %q", env.BookID, "b-1")
	}
	if env.ReportingBasis != "GAAP" {
		t.Errorf("ReportingBasis = %q, want %q", env.ReportingBasis, "GAAP")
	}
	if env.IdempotencyKey != "idem-1" {
		t.Errorf("IdempotencyKey = %q, want %q", env.IdempotencyKey, "idem-1")
	}
	if env.CausationID != "cause-1" {
		t.Errorf("CausationID = %q, want %q", env.CausationID, "cause-1")
	}
	if env.SourceSystem != "ext-system" {
		t.Errorf("SourceSystem = %q, want %q", env.SourceSystem, "ext-system")
	}
	if env.ExternalReference != "ext-ref-1" {
		t.Errorf("ExternalReference = %q, want %q", env.ExternalReference, "ext-ref-1")
	}
	if env.OccurredAt == nil || env.OccurredAt.Format(time.RFC3339) != "2026-01-15T10:30:00Z" {
		t.Errorf("OccurredAt = %v, want 2026-01-15T10:30:00Z", env.OccurredAt)
	}
	if env.EffectiveAt == nil || env.EffectiveAt.Format(time.RFC3339) != "2026-01-15T11:00:00Z" {
		t.Errorf("EffectiveAt = %v, want 2026-01-15T11:00:00Z", env.EffectiveAt)
	}
	if env.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want %q", env.Timezone, "UTC")
	}
	if env.JurisdictionContext != "US-CA" {
		t.Errorf("JurisdictionContext = %q, want %q", env.JurisdictionContext, "US-CA")
	}
	if env.PurposeContext != "audit" {
		t.Errorf("PurposeContext = %q, want %q", env.PurposeContext, "audit")
	}
	if env.ExpectedVersion != "5" {
		t.Errorf("ExpectedVersion = %q, want %q", env.ExpectedVersion, "5")
	}
	if env.WorkflowInstanceID != "wf-1" {
		t.Errorf("WorkflowInstanceID = %q, want %q", env.WorkflowInstanceID, "wf-1")
	}
	if env.ApprovalReference != "appr-1" {
		t.Errorf("ApprovalReference = %q, want %q", env.ApprovalReference, "appr-1")
	}
	if len(env.EvidenceRefs) != 3 || env.EvidenceRefs[0] != "ev-1" || env.EvidenceRefs[1] != "ev-2" || env.EvidenceRefs[2] != "ev-3" {
		t.Errorf("EvidenceRefs = %v, want [ev-1 ev-2 ev-3]", env.EvidenceRefs)
	}
}

func TestParse_OperationDefaultsToMethodPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/workflows", nil)
	env := envelope.Parse(req)
	if env.Operation != "POST /v1/workflows" {
		t.Errorf("Operation defaulted to %q, want %q", env.Operation, "POST /v1/workflows")
	}
}

func TestParse_SourceChannelCaseInsensitive(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("X-Source-Channel", "WEB")
	env := envelope.Parse(req)
	if env.SourceChannel != envelope.ChannelWeb {
		t.Errorf("SourceChannel = %q, want %q", env.SourceChannel, envelope.ChannelWeb)
	}
}

func TestParse_InvalidSourceChannel(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("X-Source-Channel", "invalid")
	env := envelope.Parse(req)
	if env.SourceChannel.Valid() {
		t.Errorf("invalid channel should not be valid")
	}
}

func TestParse_InvalidTimeFormat(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("X-Occurred-At", "not-a-timestamp")
	env := envelope.Parse(req)
	if env.OccurredAt != nil {
		t.Errorf("OccurredAt should be nil for invalid format, got %v", env.OccurredAt)
	}
}

func TestEnvelope_Actor_PrefersSubjectOverWorkload(t *testing.T) {
	e := envelope.Envelope{
		ActorSubjectID: "human-1",
		WorkloadID:     "workload-1",
	}
	if e.Actor() != "human-1" {
		t.Errorf("Actor() = %q, want %q", e.Actor(), "human-1")
	}
}

func TestEnvelope_Actor_FallsBackToWorkload(t *testing.T) {
	e := envelope.Envelope{
		WorkloadID: "workload-1",
	}
	if e.Actor() != "workload-1" {
		t.Errorf("Actor() = %q, want %q", e.Actor(), "workload-1")
	}
}

func TestEnvelope_BadTimes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req.Header.Set("X-Occurred-At", "bad")
	req.Header.Set("X-Effective-At", "also-bad")
	env := envelope.Parse(req)
	if env.OccurredAt != nil {
		t.Errorf("OccurredAt should be nil")
	}
	if env.EffectiveAt != nil {
		t.Errorf("EffectiveAt should be nil")
	}
}

func TestSourceChannel_Valid(t *testing.T) {
	for _, c := range []envelope.SourceChannel{
		envelope.ChannelWeb, envelope.ChannelMobile, envelope.ChannelAPI,
		envelope.ChannelImport, envelope.ChannelIntegration,
		envelope.ChannelScheduledJob, envelope.ChannelSystem,
	} {
		if !c.Valid() {
			t.Errorf("%q should be valid", c)
		}
	}
	if envelope.SourceChannel("invalid").Valid() {
		t.Errorf("invalid channel should not be valid")
	}
}

func TestSourceChannel_External(t *testing.T) {
	if !envelope.ChannelImport.External() {
		t.Errorf("ChannelImport should be external")
	}
	if !envelope.ChannelIntegration.External() {
		t.Errorf("ChannelIntegration should be external")
	}
	if envelope.ChannelWeb.External() {
		t.Errorf("ChannelWeb should not be external")
	}
}
