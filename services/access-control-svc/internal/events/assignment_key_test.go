package events_test

import (
	"testing"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/events"
)

// Group 1 audit gap S1-4: an assignment's grant and every revocation of it —
// request-driven, date-driven, review-driven — share one Kafka key (the
// authorization-svc assignment id), so they stay on one partition in order and
// a late grant cannot re-open a revoked projection downstream.
func TestAssignmentEventsShareTheAssignmentKey(t *testing.T) {
	a := domain.AssignmentRequest{RequestID: "req-1", AuthzAssignmentID: "asg-1", TenantID: "t"}
	granted, _ := events.AssignmentGranted(a, "approver")
	revoked, _ := events.AssignmentRevoked(a, "c", "admin", "left")
	ended, _ := events.AssignmentEnded(a, "sweep", "expired")
	byReview, _ := events.AssignmentRevokedByReview(domain.ReviewItem{AuthzAssignmentID: "asg-1", TenantID: "t"}, "c", "svc", "review")
	for name, o := range map[string]events.Outbound{"granted": granted, "revoked": revoked, "ended": ended, "review": byReview} {
		if o.Key != "asg-1" {
			t.Errorf("%s keyed %q, want the assignment id asg-1", name, o.Key)
		}
	}
	// A request never provisioned has no assignment id: keyed by the request.
	unprov, _ := events.AssignmentRevoked(domain.AssignmentRequest{RequestID: "req-2"}, "c", "a", "r")
	if unprov.Key != "req-2" {
		t.Errorf("unprovisioned request keyed %q, want req-2", unprov.Key)
	}
}
