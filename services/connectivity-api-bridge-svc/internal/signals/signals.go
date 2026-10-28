// Package signals is the operational view of the exchange platform (ZS-SVC-T-001 §17.1): connection health, inbound,
// outbound, callbacks, batch, reconciliation and security signals.
//
// Two surfaces share it. A tenant reads a Snapshot of its own signals (row-level security scopes every count to the
// tenant). The platform scrapes /metrics: process counters fed by the services' Observe hooks plus platform-wide gauges,
// with no tenant identifier and no payload in any label or value.
package signals

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Snapshot is a point-in-time read of the signal families over a window.
type Snapshot struct {
	GeneratedAt    time.Time      `json:"generated_at"`
	WindowSeconds  int            `json:"window_seconds"`
	Connections    Connections    `json:"connections"`
	Inbound        Inbound        `json:"inbound"`
	Outbound       Outbound       `json:"outbound"`
	Callbacks      Callbacks      `json:"callbacks"`
	Batch          Batch          `json:"batch"`
	Reconciliation Reconciliation `json:"reconciliation"`
	Security       Security       `json:"security"`
}

type Connections struct {
	ByStatus            map[string]int `json:"by_status"`
	ExpiringCredentials int            `json:"credentials_expiring_14d"`
}

type Inbound struct {
	Total             int            `json:"total"`
	ByState           map[string]int `json:"by_state"`
	QuarantineByClass map[string]int `json:"quarantine_by_class"`
	HandoffP50Seconds float64        `json:"handoff_latency_p50_seconds"`
	HandoffP95Seconds float64        `json:"handoff_latency_p95_seconds"`
}

type Outbound struct {
	Total                  int            `json:"total"`
	ByState                map[string]int `json:"by_state"`
	ResolutionMethods      map[string]int `json:"resolution_methods"`
	OldestAmbiguousSeconds int            `json:"oldest_ambiguous_seconds"`
	Escalated              int            `json:"escalated"`
}

type Callbacks struct {
	ByState             map[string]int `json:"by_state"`
	InvalidByReason     map[string]int `json:"invalid_by_reason"`
	NeedsReconciliation int            `json:"needs_reconciliation"`
}

type Batch struct {
	ByState                  map[string]int `json:"by_state"`
	RowsByState              map[string]int `json:"rows_by_state"`
	ManifestVariances        int            `json:"manifest_variances"`
	OldestUnprocessedSeconds int            `json:"oldest_unprocessed_seconds"`
}

type Reconciliation struct {
	RunsByState           map[string]int `json:"runs_by_state"`
	MatchRate             float64        `json:"match_rate"`
	OpenVariances         int            `json:"open_variances"`
	OpenBySeverity        map[string]int `json:"open_by_severity"`
	OldestOpenSeconds     int            `json:"oldest_open_variance_seconds"`
	RepeatedVariances     int            `json:"repeated_variances"`
	OpenByType            map[string]int `json:"open_by_type"`
	ScheduledRunsInWindow int            `json:"scheduled_runs_in_window"`
}

type Security struct {
	Events map[string]int `json:"events"`
}

// Source produces snapshots. The Postgres implementation reads the real tables.
type Source interface {
	// Snapshot reads the signals of the tenant in ctx.
	Snapshot(ctx context.Context, since time.Time) (*Snapshot, error)
	// Platform reads platform-wide gauges across tenants. Its result carries no tenant identifier.
	Platform(ctx context.Context, since time.Time) (*Snapshot, error)
}

// ── process counters ───────────────────────────────────────────────────────

// Metrics is a tiny counter registry fed by the services' Observe hooks and rendered in the Prometheus text format.
// Label values are fixed vocabulary (kinds and reason codes), never tenant, connection or payload data.
type Metrics struct {
	mu sync.Mutex
	c  map[string]uint64
}

// NewMetrics returns an empty registry.
func NewMetrics() *Metrics { return &Metrics{c: map[string]uint64{}} }

// Hook returns an Observe function for one service: every call counts xic_events_total{service, kind, reason}.
func (m *Metrics) Hook(service string) func(kind, reason string) {
	return func(kind, reason string) { m.Inc(service, kind, reason) }
}

func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// Inc counts one event.
func (m *Metrics) Inc(service, kind, reason string) {
	k := clean(service) + "\x00" + clean(kind) + "\x00" + clean(reason)
	m.mu.Lock()
	m.c[k]++
	m.mu.Unlock()
}

// Write renders the counters, then the platform gauges, in the Prometheus text exposition format.
func (m *Metrics) Write(w io.Writer, platform *Snapshot) {
	m.mu.Lock()
	keys := make([]string, 0, len(m.c))
	for k := range m.c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(w, "# HELP xic_events_total Outcomes and refusals counted by the exchange services.")
	fmt.Fprintln(w, "# TYPE xic_events_total counter")
	for _, k := range keys {
		p := strings.Split(k, "\x00")
		fmt.Fprintf(w, "xic_events_total{service=%q,kind=%q,reason=%q} %d\n", p[0], p[1], p[2], m.c[k])
	}
	m.mu.Unlock()
	if platform == nil {
		return
	}
	gauge := func(name, help string, labels map[string]int, label string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		ks := make([]string, 0, len(labels))
		for k := range labels {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, clean(k), labels[k])
		}
	}
	single := func(name, help string, v float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	gauge("xic_connections", "Connections by status.", platform.Connections.ByStatus, "status")
	single("xic_credentials_expiring_14d", "Credential bindings expiring within 14 days.", float64(platform.Connections.ExpiringCredentials))
	gauge("xic_inbound_exchanges", "Inbound exchanges in the window by state.", platform.Inbound.ByState, "state")
	gauge("xic_outbound_attempts", "Outbound attempts in the window by state.", platform.Outbound.ByState, "state")
	single("xic_outbound_oldest_ambiguous_seconds", "Age of the oldest UNKNOWN or PENDING attempt.", float64(platform.Outbound.OldestAmbiguousSeconds))
	single("xic_outbound_escalated", "Ambiguous attempts that have been escalated.", float64(platform.Outbound.Escalated))
	gauge("xic_callbacks", "Callback receipts in the window by state.", platform.Callbacks.ByState, "state")
	gauge("xic_batches", "Batches in the window by state.", platform.Batch.ByState, "state")
	single("xic_batch_oldest_unprocessed_seconds", "Age of the oldest batch not yet processed.", float64(platform.Batch.OldestUnprocessedSeconds))
	gauge("xic_reconciliation_runs", "Reconciliation runs in the window by state.", platform.Reconciliation.RunsByState, "state")
	single("xic_reconciliation_match_rate", "Share of evaluated runs that matched.", platform.Reconciliation.MatchRate)
	gauge("xic_reconciliation_open_variances", "Open variances by severity.", platform.Reconciliation.OpenBySeverity, "severity")
	single("xic_reconciliation_oldest_open_variance_seconds", "Age of the oldest open variance.", float64(platform.Reconciliation.OldestOpenSeconds))
}
