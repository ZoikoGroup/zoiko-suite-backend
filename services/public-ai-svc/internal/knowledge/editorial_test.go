package knowledge

import "testing"

func TestEditorialState(t *testing.T) {
	for _, c := range []struct {
		r     Report
		draft bool
		want  string
	}{
		{Report{State: StateStaged}, false, EditorialApproved},
		{Report{State: StateStaged}, true, EditorialPendingReview},
		{Report{State: StateHoldApproval, Reason: "not approved"}, false, EditorialPendingReview},
		{Report{State: StateHoldApproval, Reason: "high-impact review missing or overdue"}, false, EditorialValidationFailed},
		{Report{State: StateHoldApproval, Reason: "owner may not approve own source"}, false, EditorialValidationFailed},
		{Report{State: StateQuarantineIndex}, true, EditorialValidationFailed},
		{Report{State: StateRejectUnregistered}, false, EditorialValidationFailed},
	} {
		if got := EditorialState(c.r, c.draft); got != c.want {
			t.Errorf("%+v draft=%v: %s, want %s", c.r, c.draft, got, c.want)
		}
	}
}
