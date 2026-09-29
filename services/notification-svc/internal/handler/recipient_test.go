package handler_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zoiko.io/notification-svc/internal/domain"
)

func emailTo(recipient, correlationID string) map[string]any {
	return map[string]any{
		"recipient_principal_id": recipient,
		"legal_entity_id":        "le-us",
		"channel":                "EMAIL",
		"subject":                "Your payslip is available",
		"body":                   "<p>August</p>",
		"correlation_id":         correlationID,
	}
}

// The gap this closes: the register recorded who a notice was for and never
// where it went, because the stub adapter never needed an address.
func TestSend_EmailResolvesTheRecipientAddress(t *testing.T) {
	del := &stubDeliverer{delivered: true, reason: "accepted"}
	res := &stubResolver{email: "employee@example.com"}
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{}, del, res, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", emailTo("employee-9", "corr-1"), "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if n.RecipientAddress != "employee@example.com" {
		t.Errorf("recipient_address = %q, want employee@example.com", n.RecipientAddress)
	}
	if n.RecipientAddressSource != domain.AddressSourceIdentityContext {
		t.Errorf("provenance = %q, want %q", n.RecipientAddressSource, domain.AddressSourceIdentityContext)
	}
	if n.Status != "SENT" || n.ProviderResponse == "" {
		t.Errorf("status = %q, provider_response = %q — acceptance evidence was not recorded",
			n.Status, n.ProviderResponse)
	}

	// The transport must actually be handed the address, not merely have it
	// stored alongside.
	if del.seen == nil || del.seen.RecipientAddress != "employee@example.com" {
		t.Errorf("the deliverer received %+v; the resolved address did not reach the transport", del.seen)
	}
}

func TestSend_CallerSuppliedAddressIsUsedAndMarkedAsSuch(t *testing.T) {
	res := &stubResolver{email: "stored@example.com"}
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		&stubDeliverer{delivered: true}, res, "tenant-abc")

	body := emailTo("employee-9", "corr-1")
	body["recipient_address"] = "pending-org@example.com"

	rr := doReq(r, http.MethodPost, "/v1/notifications/", body, "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if n.RecipientAddress != "pending-org@example.com" {
		t.Errorf("recipient_address = %q, want the caller-supplied address", n.RecipientAddress)
	}
	// The provenance is the control. ZS-SVC-Y-001 §0.4 exists to stop a
	// free-text address being indistinguishable from a vouched-for one.
	if n.RecipientAddressSource != domain.AddressSourceRequest {
		t.Errorf("provenance = %q, want %q", n.RecipientAddressSource, domain.AddressSourceRequest)
	}
	if res.calls != 0 {
		t.Error("the identity authority was queried even though the caller supplied an address")
	}
}

// IN_APP is delivered by being recorded. Resolving an address for one would
// make every in-app notice depend on identity-context-svc being up, to compute
// a value nothing reads.
func TestSend_InAppDoesNotResolveAnAddress(t *testing.T) {
	res := &stubResolver{email: "employee@example.com"}
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		&stubDeliverer{delivered: true}, res, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", inAppTo("employee-9", "corr-1"), "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if res.calls != 0 {
		t.Error("an in-app notice triggered a recipient lookup")
	}

	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if n.RecipientAddress != "" || n.RecipientAddressSource != "" {
		t.Errorf("an in-app notice was given an address: %q / %q",
			n.RecipientAddress, n.RecipientAddressSource)
	}
}

// 03-microservices.md §9.7: notification failure must not collapse source
// operational workflows. A payroll run that finalized correctly cannot be told
// it failed because an employee has no email address on file.
func TestSend_UnresolvableRecipientIsRecordedNotRaised(t *testing.T) {
	del := &stubDeliverer{delivered: true, reason: "should not be reached"}
	res := &stubResolver{err: domain.ErrPrincipalHasNoAddress}
	pub := &stubPublisher{}
	r := newRouterFull(newStubStore(), pub, &stubAuthZ{}, del, res, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", emailTo("employee-9", "corr-1"), "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("an unresolvable recipient collapsed the caller's request: got %d, want 201: %s",
			rr.Code, rr.Body.String())
	}

	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if n.Status != "FAILED" {
		t.Errorf("status = %q, want FAILED", n.Status)
	}
	if !strings.Contains(n.FailureReason, "recipient resolution failed") {
		t.Errorf("failure_reason does not explain what happened: %q", n.FailureReason)
	}
	if del.seen != nil {
		t.Error("a notification with no resolved address was handed to the transport; " +
			"the resulting error would have blamed the mail server")
	}
	if pub.failed != 1 {
		t.Errorf("notification.failed events published = %d, want 1", pub.failed)
	}
}

// A malformed address in the request is a caller mistake, knowable without
// calling anything. Left to the provider it would come back as an SMTP
// rejection and be recorded as a delivery failure — a permanent FAILED
// blaming the mail server for a typo in the request.
func TestSend_MalformedRequestAddressIsRefusedAtTheBoundary(t *testing.T) {
	del := &stubDeliverer{delivered: true}
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		del, &stubResolver{email: "x@example.com"}, "tenant-abc")

	body := emailTo("employee-9", "corr-1")
	body["recipient_address"] = "not an address"

	rr := doReq(r, http.MethodPost, "/v1/notifications/", body, "sender-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if del.seen != nil {
		t.Error("a malformed address reached the transport")
	}
}

func TestSend_AddressOnAChannelThatHasNoEndpointIsRefused(t *testing.T) {
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		&stubDeliverer{delivered: true}, &stubResolver{}, "tenant-abc")

	body := inAppTo("employee-9", "corr-1")
	body["recipient_address"] = "employee@example.com"

	rr := doReq(r, http.MethodPost, "/v1/notifications/", body, "sender-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("an address on an IN_APP notice was accepted: got %d, want 400", rr.Code)
	}
}

// An identity-context-svc outage is not a fact about the recipient. The
// notification is still concluded — nothing retries yet — but the record must
// say which of the two happened.
func TestSend_IdentityOutageIsDistinguishableFromAMissingAddress(t *testing.T) {
	r := newRouterFull(newStubStore(), &stubPublisher{}, &stubAuthZ{},
		&stubDeliverer{delivered: true},
		&stubResolver{err: domain.ErrIdentityServiceUnavailable}, "tenant-abc")

	rr := doReq(r, http.MethodPost, "/v1/notifications/", emailTo("employee-9", "corr-1"), "sender-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.Code)
	}

	var n domain.Notification
	_ = json.Unmarshal(rr.Body.Bytes(), &n)
	if !strings.Contains(n.FailureReason, "identity-context-svc unavailable") {
		t.Errorf("an outage was not distinguishable from a recipient with no address: %q", n.FailureReason)
	}
}
