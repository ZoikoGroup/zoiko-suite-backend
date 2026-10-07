// Package siem streams security-relevant events to siem-integration-svc.
//
// SIEM export in this platform is per-tenant and opt-in: a tenant configures
// its own exporter (Splunk/Datadog/Elastic/Sentinel/Syslog endpoint) via
// siem-integration-svc, and only events for tenants with an ACTIVE exporter
// actually go anywhere. A tenant with no exporter configured is normal, not
// an error.
//
// This is fire-and-forget: streaming is a monitoring side-channel, never a
// gate. A slow or unreachable siem-integration-svc must never delay or fail
// the request that triggered the security event.
//
// ── THAT LAST PARAGRAPH USED TO BE A CLAIM, NOT A FACT ──────────────────────
//
// Stream did the work inline: an exporter lookup and then one POST per
// exporter, on the caller's goroutine, on the caller's request context, with a
// 2s HTTP timeout and a 3s context. So an unreachable siem-integration-svc
// delayed the request by however long name resolution and connect took, and
// authorization-svc calls Stream on EVERY DENIED decision — the hottest
// endpoint on the platform, on the branch that a probing or misconfigured
// caller hits repeatedly.
//
// Measured on this service, POST /v1/authorize returning DENIED, everything
// else held constant:
//
//	SIEM_SERVICE_URL empty (streaming off)          median   11 ms
//	SIEM_SERVICE_URL set, service not running       median  850 ms
//
// 850ms of a denial's latency was a best-effort telemetry sink that was not
// even there. And "not even there" is the compose DEFAULT:
// docker-compose.yml points SIEM_SERVICE_URL at siem-integration-svc, which
// lives in docker-compose.phase6.yml — so any stack without phase6 up pays
// this on every denial. That is the likeliest source of the 1.07s figure in
// the service's own performance notes, which had been attributed to the
// database.
//
// ── WHAT IT DOES NOW ────────────────────────────────────────────────────────
//
// Stream hands the event to a bounded queue and returns. A small worker pool
// does the HTTP on its own goroutines with its own background context —
// deliberately NOT the request context, which is cancelled the moment the
// response is written and would abort every delivery.
//
// The queue is BOUNDED and a full queue DROPS the event, loudly. That is the
// right trade for this signal: the alternative is an unbounded queue that
// turns a SIEM outage into this service's memory problem, and blocking is the
// behaviour being removed. A drop is counted and logged so the gap is visible
// rather than silent.
//
// Close drains the queue on shutdown so a SIGTERM does not discard events
// already accepted.
package siem

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// queueDepth and workerCount size the dispatcher.
//
// Small on purpose. This is a side-channel, so the queue exists to absorb a
// burst, not to buffer an outage: at 4 workers and a 3-second per-event
// budget, 256 slots is roughly three minutes of sustained denials before
// dropping starts, and anything longer than that is an incident the drop
// counter should be reporting rather than something to hold in memory.
const (
	queueDepth  = 256
	workerCount = 4
)

// perEventTimeout bounds one event's delivery on the worker, replacing the
// request context that used to bound it. It has to exist: without it a hung
// siem-integration-svc would occupy a worker indefinitely and the queue would
// fill behind it.
const perEventTimeout = 3 * time.Second

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

// event is one queued stream request.
type event struct {
	tenantID  string
	eventType string
	severity  Severity
	message   string
}

// Client calls siem-integration-svc. A nil/zero-value Client (baseURL == "")
// makes Stream a no-op and starts no goroutines.
type Client struct {
	baseURL   string
	sourceSvc string
	http      *http.Client
	log       *zap.Logger

	queue   chan event
	workers sync.WaitGroup

	// dropped counts events discarded because the queue was full. Exposed via
	// Dropped() so the gap is measurable rather than only logged.
	dropped atomic.Uint64

	closeOnce sync.Once
}

func New(baseURL, sourceSvc string, log *zap.Logger) *Client {
	c := &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		sourceSvc: sourceSvc,
		log:       log,
		http:      &http.Client{Timeout: 2 * time.Second},
	}
	if c.baseURL == "" {
		// Streaming disabled. No queue, no workers — Stream returns
		// immediately on the baseURL check, exactly as before.
		return c
	}
	c.queue = make(chan event, queueDepth)
	c.workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go c.worker()
	}
	return c
}

// Stream enqueues eventType for tenantID's active exporters and returns
// immediately.
//
// ctx is accepted for call-site symmetry and is deliberately NOT used for the
// delivery: it is the request context, cancelled when the response is written,
// so honouring it would cancel every event this function exists to send. The
// only thing it could legitimately gate is whether to enqueue at all, and an
// already-cancelled request is exactly when a security event is most worth
// keeping.
func (c *Client) Stream(_ context.Context, tenantID, eventType string, severity Severity, message string) {
	if c == nil || c.baseURL == "" || tenantID == "" {
		return
	}

	select {
	case c.queue <- event{tenantID: tenantID, eventType: eventType, severity: severity, message: message}:
	default:
		// Dropped rather than blocked. Blocking is the behaviour this design
		// removes, and an unbounded queue would make a SIEM outage into this
		// service's memory problem.
		n := c.dropped.Add(1)
		c.log.Warn("siem: queue full — security event dropped",
			zap.String("event_type", eventType),
			zap.Uint64("dropped_total", n))
	}
}

// Dropped returns the number of events discarded because the queue was full.
func (c *Client) Dropped() uint64 {
	if c == nil {
		return 0
	}
	return c.dropped.Load()
}

// Close stops accepting events and waits for the queued ones to drain, so a
// graceful shutdown does not discard events already accepted. Safe to call
// more than once, and on a Client with streaming disabled.
func (c *Client) Close() {
	if c == nil || c.queue == nil {
		return
	}
	c.closeOnce.Do(func() { close(c.queue) })
	c.workers.Wait()
}

func (c *Client) worker() {
	defer c.workers.Done()
	for e := range c.queue {
		c.deliver(e)
	}
}

// deliver does what Stream used to do inline.
func (c *Client) deliver(e event) {
	ctx, cancel := context.WithTimeout(context.Background(), perEventTimeout)
	defer cancel()

	exporters, err := c.activeExporters(ctx, e.tenantID)
	if err != nil {
		c.log.Debug("siem: could not list exporters — skipping (opt-in feature, not a failure)", zap.Error(err))
		return
	}
	if len(exporters) == 0 {
		return
	}

	for _, exp := range exporters {
		if err := c.streamTo(ctx, e.tenantID, exp.ID, e.eventType, e.severity, e.message); err != nil {
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
