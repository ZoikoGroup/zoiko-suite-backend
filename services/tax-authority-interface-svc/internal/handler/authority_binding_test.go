package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/tax-authority-interface-svc/internal/domain"
)

// found by the live run: the interface's authority binding was accepted on the request but never copied onto the
// interface, so every filing silently took the legacy "never transmitted" path.
func TestInterfaceAuthorityBindingIsPersistedAndValidated(t *testing.T) {
	r, _ := setupTestRouter()
	post := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/tax-authority/interfaces", bytes.NewReader(b))
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Principal-Id", "maker")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	base := func() map[string]any {
		return map[string]any{"legal_entity_id": "le-1", "jurisdiction": "GB", "authority_name": "HMRC", "protocol": "REST"}
	}

	w := post(base())
	if w.Code != http.StatusCreated {
		t.Fatalf("legacy interface: %d %s", w.Code, w.Body.String())
	}

	b := base()
	b["provider_key"] = "taxauth"
	b["connector_id"] = "7e57ab1e-0000-4000-8000-0000000000c3"
	b["certificate_ref"] = "cert-1"
	b["certificate_not_after"] = "2035-01-01T00:00:00Z"
	b["escalate_before_hours"] = 48
	w = post(b)
	if w.Code != http.StatusCreated {
		t.Fatalf("bound interface: %d %s", w.Code, w.Body.String())
	}
	var got domain.TaxInterface
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ProviderKey != "taxauth" || got.ConnectorID != "7e57ab1e-0000-4000-8000-0000000000c3" || got.CertificateRef != "cert-1" ||
		got.CertificateNotAfter == nil || got.EscalateBeforeHours != 48 {
		t.Fatalf("the binding must be on the interface: %+v", got)
	}

	for name, mut := range map[string]func(map[string]any){
		"provider without connector":  func(m map[string]any) { delete(m, "connector_id") },
		"connector is not an id":      func(m map[string]any) { m["connector_id"] = "not-a-uuid" },
		"bad provider key":            func(m map[string]any) { m["provider_key"] = "Bad Key" },
		"escalation window too large": func(m map[string]any) { m["escalate_before_hours"] = 5000 },
		"negative escalation window":  func(m map[string]any) { m["escalate_before_hours"] = -1 },
	} {
		m := base()
		m["provider_key"], m["connector_id"] = "taxauth", "7e57ab1e-0000-4000-8000-0000000000c3"
		mut(m)
		if w := post(m); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// settings without a provider are a mistake, not a setting
	for name, m := range map[string]map[string]any{
		"connector alone":   {"connector_id": "7e57ab1e-0000-4000-8000-0000000000c3"},
		"certificate alone": {"certificate_ref": "cert-1"},
		"expiry alone":      {"certificate_not_after": "2035-01-01T00:00:00Z"},
		"escalation alone":  {"escalate_before_hours": 12},
	} {
		body := base()
		for k, v := range m {
			body[k] = v
		}
		if w := post(body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}

func TestInterfaceAdapterKind(t *testing.T) {
	r, _ := setupTestRouter()
	post := func(body map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/tax-authority/interfaces", bytes.NewReader(b))
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Principal-Id", "maker")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	base := map[string]any{"legal_entity_id": "le-1", "jurisdiction": "DE", "authority_name": "Peppol AP", "protocol": "REST",
		"provider_key": "einvnet", "connector_id": "7e57ab1e-0000-4000-8000-0000000000c3"}
	with := func(kind any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		if kind != nil {
			m["adapter_kind"] = kind
		}
		return m
	}
	w := post(with("EINVOICE"))
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var got domain.TaxInterface
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.AdapterKind != "EINVOICE" {
		t.Fatalf("%+v", got)
	}
	if w := post(with("TAX")); w.Code != http.StatusCreated {
		t.Fatalf("%d", w.Code)
	}
	if w := post(with(nil)); w.Code != http.StatusCreated {
		t.Fatalf("the default kind is accepted: %d", w.Code)
	}
	if w := post(with("PAYROLL")); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown kind is refused: %d", w.Code)
	}
	legacy := map[string]any{"legal_entity_id": "le-1", "jurisdiction": "DE", "authority_name": "x", "protocol": "REST", "adapter_kind": "EINVOICE"}
	if w := post(legacy); w.Code != http.StatusBadRequest {
		t.Fatalf("an e-invoice interface needs a provider: %d", w.Code)
	}
}
