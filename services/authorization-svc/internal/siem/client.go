// Package siem streams security-relevant events to siem-integration-svc.
//
// SIEM export in this platform is per-tenant and opt-in: a tenant configures
// its own exporter (Splunk/Datadog/Elastic/Sentinel/Syslog endpoint) via
// siem-integration-svc, and only events for tenants with an ACTIVE exporter
// actually go anywhere. A tenant with no exporter configured is normal, not
// an error.
//
// This is deliberately fire-and-forget: streaming is a monitoring
// side-channel, never a gate. A slow or unreachable siem-integration-svc
// must never delay or fail the request that triggered the security event.
package siem

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

type exporter struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type exportersResponse struct {
	Data []exporter `json:"data"`
}

type streamRequest struct {
	ExporterID string   `json:"exporter_id"`
	SourceSvc  string   `json:"source_service"`
	EventType  string   `json:"event_type"`
	Severity   Severity `json:"severity"`
	Message    string   `json:"message"`
	Payload    string   `json:"payload,omitempty"`
}

// Client calls siem-integration-svc. A nil/zero-value Client (baseURL == "")
// makes Stream a no-op.
type Client struct {
	baseURL   string
	sourceSvc string
	http      *http.Client
	log       *zap.Logger
}

func New(baseURL, sourceSvc string, log *zap.Logger) *Client {
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		sourceSvc: sourceSvc,
		log:       log,
		http:      &http.Client{Timeout: 2 * time.Second},
	}
}

// Stream looks up tenantID's active SIEM exporters and streams eventType to
// each.
func (c *Client) Stream(ctx context.Context, tenantID, eventType string, severity Severity, message string) {
	if c == nil || c.baseURL == "" || tenantID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	exporters, err := c.activeExporters(ctx, tenantID)
	if err != nil {
		c.log.Debug("siem: could not list exporters — skipping (opt-in feature, not a failure)", zap.Error(err))
		return
	}
	if len(exporters) == 0 {
		return
	}

	for _, exp := range exporters {
		if err := c.streamTo(ctx, tenantID, exp.ID, eventType, severity, message); err != nil {
			c.log.Warn("siem: stream failed for one exporter", zap.String("exporter_id", exp.ID), zap.Error(err))
		}
	}
}

func (c *Client) activeExporters(ctx context.Context, tenantID string) ([]exporter, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/siem/exporters", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var out exportersResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	active := make([]exporter, 0, len(out.Data))
	for _, e := range out.Data {
		if e.Status == "ACTIVE" {
			active = append(active, e)
		}
	}
	return active, nil
}

func (c *Client) streamTo(ctx context.Context, tenantID, exporterID, eventType string, severity Severity, message string) error {
	body, err := json.Marshal(streamRequest{
		ExporterID: exporterID,
		SourceSvc:  c.sourceSvc,
		EventType:  eventType,
		Severity:   severity,
		Message:    message,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/siem/stream", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}
