package domain

import "time"

// CountAmount is a count and exact exposure for one grouping key and currency.
type CountAmount struct {
	Key      string `json:"key"`
	Currency string `json:"currency"`
	Count    int    `json:"count"`
	Exposure string `json:"exposure"`
}

// MonitoringSnapshot is the ZS-CONTROL-001 s28 signal set for one entity (and optionally
// one period), computed on read from authoritative rows. It informs; it never replaces
// certification authority.
type MonitoringSnapshot struct {
	LegalEntityID string    `json:"legal_entity_id"`
	PeriodID      string    `json:"period_id,omitempty"`
	AsOf          time.Time `json:"as_of"`

	Completion struct {
		DefinitionsTotal      int `json:"definitions_total"`
		WithRun               int `json:"controls_with_a_run"`
		Executed              int `json:"controls_executed"`
		Certified             int `json:"controls_certified"`
		FailedOrIndeterminate int `json:"failed_or_indeterminate"`
	} `json:"control_completion"`

	RunDuration struct {
		AvgSeconds             float64 `json:"avg_seconds"`
		MaxSeconds             float64 `json:"max_seconds"`
		Samples                int     `json:"samples"`
		OldestUnstartedSeconds float64 `json:"oldest_unstarted_run_seconds"`
	} `json:"run_duration_and_lag"`

	Exceptions struct {
		Total       int `json:"total"`
		Unresolved  int `json:"unresolved"`
		SLABreached int `json:"sla_breached"`
		Recurring   int `json:"recurring"`
		LateData    int `json:"late_data"`
	} `json:"exceptions"`

	Aging struct {
		UpTo7Days  int `json:"up_to_7_days"`
		Days8To30  int `json:"days_8_to_30"`
		Days31To90 int `json:"days_31_to_90"`
		Over90Days int `json:"over_90_days"`
	} `json:"unresolved_exception_aging"`

	ExceptionsByAssertion []CountAmount `json:"unresolved_by_assertion"`
	ExceptionsByCategory  []CountAmount `json:"unresolved_by_category"`
	RootCauses            []CountAmount `json:"root_causes"`

	CertificationLatency struct {
		AvgSeconds float64 `json:"avg_seconds"`
		Samples    int     `json:"samples"`
	} `json:"certification_latency"`

	// Rates are nil when their denominator is zero: "no data" is not "0%".
	Rates struct {
		ControlCompletion    *float64 `json:"control_completion_rate"`
		ControlCertification *float64 `json:"control_certification_rate"`
		FailureIndeterminate *float64 `json:"failure_or_indeterminate_rate"`
		RecurringException   *float64 `json:"recurring_exception_rate"`
		LateData             *float64 `json:"late_data_rate"`
	} `json:"rates"`

	// Signals the platform cannot honestly produce, with the reason.
	Unavailable map[string]string `json:"unavailable"`
}

// UnavailableSignals names the s28 signals this build cannot compute and why.
func UnavailableSignals() map[string]string {
	return map[string]string{
		"manual_match_rate": "matching is fully automatic; no manual-match action exists to measure",
		"suspense_aging":    "FIN-CTRL-031 is BLOCKED: no suspense designation or item-level clearing in the ledger",
	}
}

func ratio(n, d int) *float64 {
	if d <= 0 {
		return nil
	}
	v := float64(n) / float64(d)
	return &v
}

// Derive fills the rates from the counts.
func (m *MonitoringSnapshot) Derive() {
	m.Rates.ControlCompletion = ratio(m.Completion.Executed, m.Completion.DefinitionsTotal)
	m.Rates.ControlCertification = ratio(m.Completion.Certified, m.Completion.DefinitionsTotal)
	m.Rates.FailureIndeterminate = ratio(m.Completion.FailedOrIndeterminate, m.Completion.WithRun)
	m.Rates.RecurringException = ratio(m.Exceptions.Recurring, m.Exceptions.Total)
	m.Rates.LateData = ratio(m.Exceptions.LateData, m.Exceptions.Total)
}
