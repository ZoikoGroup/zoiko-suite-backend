package consentregistry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/privacy-decision-svc/internal/consentregistry"
)

func TestConsentRegistryClient_ResolveStatus(t *testing.T) {
	for _, tc := range []struct {
		status          string
		receiptID       string
		noticeVersionID string
	}{
		{"GRANTED", "rcpt-1", "nv-1"},
		{"DENIED", "", ""},
		{"WITHDRAWN", "rcpt-2", "nv-1"},
		{"EXPIRED", "rcpt-3", "nv-1"},
		{"INVALIDATED", "rcpt-4", "nv-1"},
		{"NOT_REQUESTED", "", ""},
	} {
		t.Run(tc.status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Tenant-Id") != "tenant-1" {
					t.Errorf("expected X-Tenant-Id tenant-1, got %s", r.Header.Get("X-Tenant-Id"))
				}
				if r.Header.Get("X-Principal-Id") != "principal-1" {
					t.Errorf("expected X-Principal-Id principal-1, got %s", r.Header.Get("X-Principal-Id"))
				}
				if r.URL.Query().Get("subject_ref") != "subj-1" || r.URL.Query().Get("purpose_id") != "purp-1" {
					t.Errorf("unexpected query params: %s", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{
					"subject_ref": "subj-1",
					"purpose_id": "purp-1",
					"status": "` + tc.status + `",
					"latest_receipt": {
						"consent_receipt_id": "` + tc.receiptID + `",
						"notice_version_id": "` + tc.noticeVersionID + `"
					}
				}`))
			}))
			defer server.Close()

			c := consentregistry.NewClient(server.URL)
			res, err := c.ResolveStatus(context.Background(), "tenant-1", "principal-1", "subj-1", "purp-1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Status != tc.status {
				t.Errorf("expected status %s, got %s", tc.status, res.Status)
			}
			if tc.receiptID != "" {
				if res.LatestReceipt == nil || res.LatestReceipt.ConsentReceiptID != tc.receiptID {
					t.Errorf("expected receipt %s, got %v", tc.receiptID, res.LatestReceipt)
				}
			}
		})
	}
}

func TestConsentRegistryClient_ErrorHandling(t *testing.T) {
	// 401 Unauthorized returns ErrUnavailable
	server401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server401.Close()

	c := consentregistry.NewClient(server401.URL)
	_, err := c.ResolveStatus(context.Background(), "t", "p", "s", "p")
	if err != consentregistry.ErrUnavailable {
		t.Errorf("expected ErrUnavailable on 401, got %v", err)
	}

	// 503 Service Unavailable returns ErrUnavailable
	server503 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server503.Close()

	c2 := consentregistry.NewClient(server503.URL)
	_, err2 := c2.ResolveStatus(context.Background(), "t", "p", "s", "p")
	if err2 != consentregistry.ErrUnavailable {
		t.Errorf("expected ErrUnavailable on 503, got %v", err2)
	}
}
