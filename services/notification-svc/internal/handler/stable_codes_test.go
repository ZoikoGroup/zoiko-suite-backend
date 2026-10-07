package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/domain"
)

type apiError struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"error_message"`
	Reason    string `json:"reason"`
	Stable    string `json:"reason_code"`
}

func decodeAPIError(t *testing.T, body []byte) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error body: %q", body)
	}
	return e
}

// The stable code rides BESIDE the service's own error code and never replaces it.
func TestStableCodes_ApiErrorsCarryTheStableCodeBesideTheOwn(t *testing.T) {
	rig := newNoticeRig(t, true, true)

	rr := doReq(rig.r, http.MethodGet, "/v1/communication-intents/"+uuid.NewString()+"/effective", nil, "alice")
	e := decodeAPIError(t, rr.Body.Bytes())
	if e.ErrorCode != "intent_not_found" || e.Stable != "NCD-001" || e.Reason != "INTENT_NOT_FOUND" {
		t.Errorf("unknown intent: %+v", e)
	}

	// A generic failure is never given a stable code it does not deserve.
	rr = doReq(rig.r, http.MethodPost, "/v1/notices/", map[string]any{}, "sender-1")
	e = decodeAPIError(t, rr.Body.Bytes())
	if e.ErrorCode != "missing_fields" || e.Stable != "" {
		t.Errorf("a missing-fields refusal must carry no stable code: %+v", e)
	}
}

// NP-09 / NCD-005: a template published in another language is an unapproved LOCALE, not an
// unpublished template, and no other language is substituted.
func TestStableCodes_AnUnapprovedLocaleIsNotAnUnpublishedTemplate(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	templateID := publishTestTemplate(t, r, "le-us", "en-US", "<p>Hello</p>", nil)

	rr := doReq(r, http.MethodPost, "/v1/notifications/", map[string]any{
		"recipient_principal_id": "principal-2", "legal_entity_id": "le-us", "channel": "EMAIL",
		"subject": "Hi", "template_id": templateID, "locale": "fr-FR", "correlation_id": "corr-locale-1",
		"source_event_type": "x.y", "source_reference": "ref-1",
	}, "sender-1")
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	e := decodeAPIError(t, rr.Body.Bytes())
	if e.ErrorCode != "locale_not_approved" || e.Stable != "NCD-005" || !strings.Contains(e.Message, "NCD-005 LOCALE_NOT_APPROVED") {
		t.Errorf("unexpected error: %+v", e)
	}
	if !strings.Contains(e.Message, "fr-FR") {
		t.Error("the refusal should name the locale that was asked for")
	}
}

// NCD-006: an unresolvable recipient is recorded with the stable code in its reason.
func TestStableCodes_UnresolvedRecipientReasonCarriesNCD006(t *testing.T) {
	res := &stubResolver{err: domain.ErrPrincipalHasNoAddress}
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubDeliverer{delivered: true}, res, "tenant-abc")
	rr := doReq(r, http.MethodPost, "/v1/notifications/", emailTo("employee-9", "corr-ncd006"), "sender-1")
	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if !strings.HasPrefix(n.FailureReason, "NCD-006 RECIPIENT_UNRESOLVED:") {
		t.Errorf("failure reason = %q", n.FailureReason)
	}
}

// NCD-016: a response that arrives before delivery was evidenced is refused with the code.
func TestStableCodes_ResponseBeforeDeliveryIsEvidenceInsufficient(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, n := rig.create(t, nil) // READY: never dispatched, so nothing has been delivered
	rr := doReq(rig.r, http.MethodPost, "/v1/notices/"+n.NoticeID+"/acknowledgement", map[string]any{"action": "ACKNOWLEDGE"}, "recipient-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	e := decodeAPIError(t, rr.Body.Bytes())
	if e.ErrorCode != "evidence_insufficient" || e.Stable != "NCD-016" {
		t.Errorf("unexpected error: %+v", e)
	}
}

// NCD-017: an acknowledgement that does not fit, or that nobody may make.
func TestStableCodes_InvalidAcknowledgementsCarryNCD017(t *testing.T) {
	rig := newNoticeRig(t, true, true)
	_, n := rig.create(t, nil)
	rig.notices.notices[n.NoticeID].Status = domain.NoticeAckPending
	path := "/v1/notices/" + n.NoticeID + "/acknowledgement"

	for name, tc := range map[string]struct {
		who, action, want string
		status            int
	}{
		"an operator":                {"operator-9", "ACKNOWLEDGE", "operator_acknowledgement_refused", http.StatusForbidden},
		"an action that doesn't fit": {"recipient-1", "DECLINE", "acknowledgement_invalid", http.StatusBadRequest},
	} {
		rr := doReq(rig.r, http.MethodPost, path, map[string]any{"action": tc.action}, tc.who)
		e := decodeAPIError(t, rr.Body.Bytes())
		if rr.Code != tc.status || e.ErrorCode != tc.want || e.Stable != "NCD-017" {
			t.Errorf("%s: %d %+v", name, rr.Code, e)
		}
	}
}
