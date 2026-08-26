package domain

import (
	"fmt"
	"time"
)

type SIEMPlatform string
type EventSeverity string
type ExporterStatus string

const (
	PlatformSplunk   SIEMPlatform = "SPLUNK"
	PlatformDatadog  SIEMPlatform = "DATADOG"
	PlatformElastic  SIEMPlatform = "ELASTIC"
	PlatformSentinel SIEMPlatform = "SENTINEL"
	PlatformSyslog   SIEMPlatform = "SYSLOG"
)

const (
	SeverityLow      EventSeverity = "LOW"
	SeverityMedium   EventSeverity = "MEDIUM"
	SeverityHigh     EventSeverity = "HIGH"
	SeverityCritical EventSeverity = "CRITICAL"
)

const (
	ExporterActive   ExporterStatus = "ACTIVE"
	ExporterPaused   ExporterStatus = "PAUSED"
	ExporterDisabled ExporterStatus = "DISABLED"
)

type SIEMExporter struct {
	ID            string       `json:"id"`
	TenantID      string       `json:"tenant_id"`
	LegalEntityID string       `json:"legal_entity_id"`
	Name          string       `json:"name"`
	Platform      SIEMPlatform `json:"platform"`
	EndpointURL   string       `json:"endpoint_url"`
	// AuthToken is the credential for the tenant's SIEM platform. It is
	// json:"-" so it can NEVER be serialised, on any route, present or
	// future — a structural fix rather than a per-handler one.
	//
	// It was previously json:"auth_token,omitempty", and both GetExporter
	// and ListExporters return this struct directly, so any caller holding
	// the tenant header read the live token back out. Redacting at each
	// call site would have worked today and silently regressed the first
	// time someone added a fourth read route; the wire format simply
	// cannot carry it now. Nothing needs it there: CreateExporterRequest
	// carries its own AuthToken field for input, and this struct is never
	// JSON round-tripped for persistence (the store is in-memory).
	//
	// Two deeper problems this does NOT fix, both tracked as row 8p-a:
	//
	//  1. The token is stored in plaintext. Doc 05 §13 calls for a vault
	//     reference rather than the secret itself, which needs
	//     secret-vault-integration-svc to be able to return material —
	//     blocked on row 81.
	//  2. Nothing ever READS this field. StreamEvent persists events
	//     locally and never authenticates to the SIEM platform, so the
	//     service currently collects and holds a live third-party
	//     credential it has no functional use for. Until egress exists,
	//     the safest version of this service would not accept the token at
	//     all.
	AuthToken    string         `json:"-"`
	Status       ExporterStatus `json:"status"`
	EventsSent   int64          `json:"events_sent"`
	LastStreamed *time.Time     `json:"last_streamed,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

type SIEMEvent struct {
	ID         string        `json:"id"`
	TenantID   string        `json:"tenant_id"`
	ExporterID string        `json:"exporter_id"`
	SourceSvc  string        `json:"source_service"`
	EventType  string        `json:"event_type"`
	Severity   EventSeverity `json:"severity"`
	Message    string        `json:"message"`
	Payload    string        `json:"payload,omitempty"`
	Status     string        `json:"status"` // DELIVERED, FAILED
	Timestamp  time.Time     `json:"timestamp"`
}

type CreateExporterRequest struct {
	LegalEntityID string       `json:"legal_entity_id"`
	Name          string       `json:"name"`
	Platform      SIEMPlatform `json:"platform"`
	EndpointURL   string       `json:"endpoint_url"`
	AuthToken     string       `json:"auth_token,omitempty"`
}

type StreamEventRequest struct {
	ExporterID string        `json:"exporter_id"`
	SourceSvc  string        `json:"source_service"`
	EventType  string        `json:"event_type"`
	Severity   EventSeverity `json:"severity"`
	Message    string        `json:"message"`
	Payload    string        `json:"payload,omitempty"`
}

func (r *CreateExporterRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	if r.Platform == "" {
		return fmt.Errorf("platform is required")
	}
	if r.EndpointURL == "" {
		return fmt.Errorf("endpoint_url is required")
	}
	return nil
}

func (r *StreamEventRequest) Validate() error {
	if r.ExporterID == "" {
		return fmt.Errorf("exporter_id is required")
	}
	if r.SourceSvc == "" {
		return fmt.Errorf("source_service is required")
	}
	if r.EventType == "" {
		return fmt.Errorf("event_type is required")
	}
	if r.Message == "" {
		return fmt.Errorf("message is required")
	}
	if r.Severity == "" {
		r.Severity = SeverityMedium
	}
	return nil
}
