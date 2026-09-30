// COM-04 Usage Metering (ZS-SVC-Q-001 §4.4). Measures and certifies billable
// platform usage under registered meter definitions.
//
// COM-04 deliberately does not know about price components or subscription
// items (§4.4 "Must not own": product price, subscription). It resolves
// only which subscription TERM an event's occurred_at falls in — one usage
// window per subscription term, reusing COM-02's own term boundaries rather
// than keeping a second calendar. Which meter a price component bills
// against is read by COM-05 when it rates a certified statement.
package domain

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"zoiko.io/commercial-account-svc/internal/money"
)

var dimensionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const (
	PrefixUsageStatement  = "cust_"
	PrefixUsageAdjustment = "cuadj_"
)

type StatementStatus string

const (
	StatementOpen       StatementStatus = "OPEN"
	StatementFrozen     StatementStatus = "FROZEN"
	StatementCertified  StatementStatus = "CERTIFIED"
	StatementAdjusted   StatementStatus = "ADJUSTED"
	StatementSuperseded StatementStatus = "SUPERSEDED"
)

type UsageEventStatus string

const (
	UsageAccepted    UsageEventStatus = "ACCEPTED"
	UsageQuarantined UsageEventStatus = "QUARANTINED"
	UsageCorrected   UsageEventStatus = "CORRECTED"
)

// MeterDefinition is the registered, immutable-once-created contract for one
// billable metric. A new version is how a meter's aggregation changes;
// meter_key/meter_version together are what a price component (COM-01) and
// an ingested event both bind to.
type MeterDefinition struct {
	MeterKey                string     `json:"meter_key"`
	MeterVersion            int        `json:"meter_version"`
	DisplayName             string     `json:"display_name"`
	Unit                    string     `json:"unit"`
	AggregationMethod       string     `json:"aggregation_method"`
	UniqueDimension         *string    `json:"unique_dimension,omitempty"`
	AllowNegativeCorrection bool       `json:"allow_negative_correction"`
	CreatedAt               time.Time  `json:"created_at"`
	CreatedByPrincipalID    string     `json:"created_by_principal_id"`
	RetiredAt               *time.Time `json:"retired_at,omitempty"`
	RetiredByPrincipalID    *string    `json:"retired_by_principal_id,omitempty"`
	RetireReason            *string    `json:"retire_reason,omitempty"`
}

// UsageEventRecord is one ingested (or attempted) usage fact.
type UsageEventRecord struct {
	MeterKey            string            `json:"meter_key"`
	UsageEventID        string            `json:"usage_event_id"`
	MeterVersion        int               `json:"meter_version"`
	OrganizationID      string            `json:"organization_id"`
	SubscriptionID      string            `json:"subscription_id"`
	TermNo              int               `json:"term_no"`
	StatementID         *string           `json:"statement_id,omitempty"`
	Quantity            string            `json:"quantity"`
	Dimensions          map[string]string `json:"dimensions"`
	OccurredAt          time.Time         `json:"occurred_at"`
	ObservedAt          time.Time         `json:"observed_at"`
	SourceService       string            `json:"source_service"`
	Status              UsageEventStatus  `json:"status"`
	QuarantineReason    *string           `json:"quarantine_reason,omitempty"`
	Late                bool              `json:"late"`
	SupersededByEventID *string           `json:"superseded_by_event_id,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
}

// UsageStatement is the frozen/certified population for one subscription
// term under one meter.
type UsageStatement struct {
	StatementID             string          `json:"statement_id"`
	SubscriptionID          string          `json:"subscription_id"`
	TermNo                  int             `json:"term_no"`
	MeterKey                string          `json:"meter_key"`
	MeterVersion            int             `json:"meter_version"`
	Status                  StatementStatus `json:"status"`
	TotalQuantity           string          `json:"total_quantity"`
	EventCount              int             `json:"event_count"`
	QuarantinedCount        int             `json:"quarantined_count"`
	WatermarkAt             *time.Time      `json:"watermark_at,omitempty"`
	CreatedAt               time.Time       `json:"created_at"`
	FrozenAt                *time.Time      `json:"frozen_at,omitempty"`
	FrozenByPrincipalID     *string         `json:"frozen_by_principal_id,omitempty"`
	CertifiedAt             *time.Time      `json:"certified_at,omitempty"`
	CertifiedByPrincipalID  *string         `json:"certified_by_principal_id,omitempty"`
	SupersededAt            *time.Time      `json:"superseded_at,omitempty"`
	SupersededByPrincipalID *string         `json:"superseded_by_principal_id,omitempty"`
	SupersedeReason         *string         `json:"supersede_reason,omitempty"`
	SupersededByStatementID *string         `json:"superseded_by_statement_id,omitempty"`
}

// UsageAdjustment carries a late event's quantity into the next OPEN
// statement instead of rewriting the CERTIFIED/ADJUSTED one it would
// otherwise have belonged to. OccurredAt/Dimensions are the same evidence a
// normal AcceptedEvent carries — required so the adjustment can be fed into
// domain.Aggregate exactly as if it had arrived on time, correctly for
// every aggregation method (MAX/LAST need OccurredAt; UNIQUE_COUNT needs
// Dimensions), not just SUM.
type UsageAdjustment struct {
	AdjustmentID         string            `json:"adjustment_id"`
	OriginStatementID    string            `json:"origin_statement_id"`
	TargetStatementID    string            `json:"target_statement_id"`
	MeterKey             string            `json:"meter_key"`
	SourceUsageEventID   string            `json:"source_usage_event_id"`
	Quantity             string            `json:"quantity"`
	OccurredAt           time.Time         `json:"occurred_at"`
	Dimensions           map[string]string `json:"dimensions"`
	Reason               string            `json:"reason"`
	CreatedAt            time.Time         `json:"created_at"`
	CreatedByPrincipalID string            `json:"created_by_principal_id"`
}

var aggregationMethodSet = map[string]bool{"SUM": true, "MAX": true, "LAST": true, "UNIQUE_COUNT": true}

// ValidateMeterDefinition checks a new meter registration.
func ValidateMeterDefinition(m *MeterDefinition) error {
	if !meterKeyPattern.MatchString(m.MeterKey) {
		return invalid("meter_key", "must be 1-128 chars of lowercase letters, digits and . _ : -")
	}
	if !aggregationMethodSet[m.AggregationMethod] {
		return invalid("aggregation_method", "must be SUM, MAX, LAST or UNIQUE_COUNT")
	}
	if strEmpty(m.DisplayName) {
		return invalid("display_name", "is required")
	}
	if strEmpty(m.Unit) {
		return invalid("unit", "is required")
	}
	hasDim := m.UniqueDimension != nil
	if (m.AggregationMethod == "UNIQUE_COUNT") != hasDim {
		return invalid("unique_dimension", "is required for UNIQUE_COUNT, and only for UNIQUE_COUNT")
	}
	if hasDim && !dimensionKeyPattern.MatchString(*m.UniqueDimension) {
		return invalid("unique_dimension", "must be 1-64 chars of lowercase letters, digits and underscores")
	}
	return nil
}

func strEmpty(s string) bool { return len(strings.TrimSpace(s)) == 0 }

// ── Ingestion validation ─────────────────────────────────────────────────────

// EventInput is what a caller supplies to RegisterUsageEvent, before it is
// checked against the meter it claims.
type EventInput struct {
	Quantity   string
	Dimensions map[string]string
	OccurredAt time.Time
}

// ValidateUsageQuantity applies the meter's own validity rules (COM-CTRL-016;
// negative path #16): a plain SUM/MAX/LAST meter takes any non-negative
// exact decimal; UNIQUE_COUNT takes exactly quantity "1" and requires its
// declared dimension to be present. It returns the quarantine reason, or ""
// if the event is valid.
func ValidateUsageQuantity(m MeterDefinition, in EventInput) string {
	q, err := money.Parse(in.Quantity)
	if err != nil {
		return "quantity must be a non-negative decimal with at most 4 fractional digits"
	}
	if m.AggregationMethod == "UNIQUE_COUNT" {
		one, _ := money.Parse("1")
		if q.Cmp(one) != 0 {
			return "a UNIQUE_COUNT meter counts occurrences; quantity must be exactly 1"
		}
		if _, ok := in.Dimensions[*m.UniqueDimension]; !ok {
			return "dimensions." + *m.UniqueDimension + " is required by this meter's unique_dimension"
		}
	}
	return ""
}

// ── Aggregation ──────────────────────────────────────────────────────────────

// AcceptedEvent is the minimal shape aggregation needs.
type AcceptedEvent struct {
	Quantity   string
	Dimensions map[string]string
	OccurredAt time.Time
	EventID    string // tie-break for LAST: lexically greatest wins on an exact tie
}

// Aggregate computes a statement's total from its accepted events, exactly,
// under the meter's aggregation method — the one place the "Aggregation"
// certification assertion (§4.4: "formula ... versioned and reproducible")
// is implemented, so CertifyUsageStatement and ExplainAggregation can share
// it and never disagree.
func Aggregate(m MeterDefinition, events []AcceptedEvent) string {
	if len(events) == 0 {
		return "0"
	}
	switch m.AggregationMethod {
	case "SUM":
		total := money.MustParse("0").Rat()
		for _, e := range events {
			total.Add(total, money.MustParse(e.Quantity).Rat())
		}
		return money.FormatHalfEven(total, money.MaxScale)
	case "MAX":
		max := money.MustParse(events[0].Quantity)
		for _, e := range events[1:] {
			q := money.MustParse(e.Quantity)
			if q.Cmp(max) > 0 {
				max = q
			}
		}
		return max.String()
	case "LAST":
		sorted := append([]AcceptedEvent(nil), events...)
		sort.Slice(sorted, func(i, j int) bool {
			if !sorted[i].OccurredAt.Equal(sorted[j].OccurredAt) {
				return sorted[i].OccurredAt.Before(sorted[j].OccurredAt)
			}
			return sorted[i].EventID < sorted[j].EventID
		})
		return sorted[len(sorted)-1].Quantity
	case "UNIQUE_COUNT":
		seen := map[string]bool{}
		for _, e := range events {
			seen[e.Dimensions[*m.UniqueDimension]] = true
		}
		return strconv.Itoa(len(seen))
	}
	return "0"
}

var (
	ErrMeterDefinitionNotFound     = errorString("meter definition not found")
	ErrMeterDefinitionExists       = errorString("this meter key already has a version at that number")
	ErrMeterDefinitionRetired      = errorString("meter definition is retired")
	ErrUsageEventExists            = errorString("this usage event was already recorded")
	ErrUsageEventNotFound          = errorString("usage event not found")
	ErrUsageEventNotCorrectable    = errorString("only an accepted event in an open window can be corrected")
	ErrNoOpenTermForCorrection     = errorString("the subscription has no open term to carry a late correction into")
	ErrStatementNotFound           = errorString("usage statement not found")
	ErrStatementInvalidState       = errorString("usage statement is not in a state that allows this action")
	ErrStatementNotOpenForWindow   = errorString("the subscription has no matching open term for this event's occurred_at")
	ErrReopenNeedsIndependentActor = errorString("reopening a statement needs a principal other than whoever certified it")
	ErrAdjustmentSourceUnlinked    = errorString("the source event has no statement linkage to use as an adjustment's origin")
)
