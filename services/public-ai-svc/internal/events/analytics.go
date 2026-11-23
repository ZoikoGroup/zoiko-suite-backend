package events

import (
	"errors"
	"time"

	"zoiko.io/public-ai-svc/internal/store"
)

// TypeAnalytics carries the Doc 02 §27 analytics events. One CloudEvents
// type with an enum name keeps consumers simple; every field is an id, an
// enum or a coarse bucket. Raw questions, URLs, IPs, contact details,
// evidence snippets and model output are never fields (Doc 02 §27
// "prohibited payload", Doc 06 §15).
const TypeAnalytics = "chat.analytics.v1"

// Analytics event names (Doc 02 §27).
const (
	ChatOpened         = "chat_opened"
	IntentClassified   = "intent_classified"
	ClarificationShown = "clarification_shown"
	AnswerDelivered    = "answer_delivered"
	SourceOpened       = "source_opened"
	CTAOpened          = "cta_opened"
	HandoffOffered     = "handoff_offered"
	FeedbackSubmitted  = "feedback_submitted"
	SafetyIntercepted  = "safety_intercepted"
	SourceConflict     = "source_conflict"
)

// AnalyticsNames is the allowlist of event names.
var AnalyticsNames = map[string]bool{
	ChatOpened: true, IntentClassified: true, ClarificationShown: true, AnswerDelivered: true, SourceOpened: true,
	CTAOpened: true, HandoffOffered: true, FeedbackSubmitted: true, SafetyIntercepted: true, SourceConflict: true,
}

// Analytics is the event payload. Only the fields its name permits are set.
type Analytics struct {
	Name string `json:"name"`
	// chat_opened: route_id (the page_context enum), locale, release_version.
	RouteID        string `json:"route_id,omitempty"`
	Locale         string `json:"locale,omitempty"`
	ReleaseVersion string `json:"release_version,omitempty"`
	// intent_classified: intent_id, confidence bucket, safety disposition.
	IntentID          string `json:"intent_id,omitempty"`
	ConfidenceBucket  string `json:"confidence_bucket,omitempty"`
	SafetyDisposition string `json:"safety_disposition,omitempty"`
	// clarification_shown: slot_name, reason_code.
	SlotName   string `json:"slot_name,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	// answer_delivered: answer_class, source count, status class, latency band.
	AnswerClass   string `json:"answer_class,omitempty"`
	SourceCount   *int   `json:"source_count,omitempty"`
	StatusClass   string `json:"status_class,omitempty"`
	LatencyBucket string `json:"latency_bucket,omitempty"`
	// source_opened: approved link_id (the citation url_key); cta_opened: action_id (route key).
	LinkID   string `json:"link_id,omitempty"`
	ActionID string `json:"action_id,omitempty"`
	// handoff_offered: handoff category, route availability.
	HandoffCategory string `json:"handoff_category,omitempty"`
	RouteAvailable  *bool  `json:"route_available,omitempty"`
	// feedback_submitted: helpfulness enum, optional controlled reason.
	Helpfulness    string `json:"helpfulness,omitempty"`
	FeedbackReason string `json:"feedback_reason,omitempty"`
	// safety_intercepted: reason code (above), severity bucket.
	SeverityBucket string `json:"severity_bucket,omitempty"`
	// source_conflict: conflict ids and the owner queue.
	ConflictIDs  []string `json:"conflict_ids,omitempty"`
	OwnerQueueID string   `json:"owner_queue_id,omitempty"`
}

var errAnalyticsName = errors.New("events: unknown analytics event name")

// AnalyticsEvent builds one §27 event.
func AnalyticsEvent(a Analytics, at time.Time) (store.OutboxEvent, error) {
	if !AnalyticsNames[a.Name] {
		return store.OutboxEvent{}, errAnalyticsName
	}
	return build(TopicTelemetry, TypeAnalytics, a, at)
}

// IntPtr and BoolPtr keep zero values explicit (a source count of 0 is data).
func IntPtr(n int) *int    { return &n }
func BoolPtr(b bool) *bool { return &b }
