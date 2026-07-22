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
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	Name          string         `json:"name"`
	Platform      SIEMPlatform   `json:"platform"`
	EndpointURL   string         `json:"endpoint_url"`
	AuthToken     string         `json:"auth_token,omitempty"`
	Status        ExporterStatus `json:"status"`
	EventsSent    int64          `json:"events_sent"`
	LastStreamed  *time.Time     `json:"last_streamed,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
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
