package events_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

func TestSubmissionEventType_MapsReceiptStatesToTheS26Names(t *testing.T) {
	cases := []struct{ kind, status, want string }{
		{"FILING", "ACCEPTED", "filing.accepted"},
		{"FILING", "FILED", "filing.accepted"},
		{"FILING", "REJECTED", "filing.rejected"},
		{"FILING", "PROCESSING", "filing.status-changed"},
		{"EINVOICE", "ACCEPTED", "einvoice.accepted"},
		{"EINVOICE", "CLEARED", "einvoice.accepted"},
		{"EINVOICE", "REJECTED", "einvoice.rejected"},
		{"EINVOICE", "SOME_CUSTOM_STATE", "einvoice.status-changed"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, events.SubmissionEventType(c.kind, c.status), "%s %s", c.kind, c.status)
	}
	assert.Equal(t, "filing.submitted", events.SubmissionRegisteredEventType("FILING"))
	assert.Equal(t, "einvoice.submitted", events.SubmissionRegisteredEventType("EINVOICE"))
}

func TestWave8EventNamesAreStable(t *testing.T) {
	assert.Equal(t, "jurisdiction.support-changed", events.EventJurisdictionSupportChange)
	assert.Equal(t, "regulatory-obligation.due-date-calculated", events.EventObligationDueDated)
}
