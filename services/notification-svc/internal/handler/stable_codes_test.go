package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
