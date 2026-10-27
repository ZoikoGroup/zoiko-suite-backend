package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zoiko.io/privacy-consent-svc/internal/domain"
)

func prefBody(value string, effective string) string {
	b := `{"subject_ref":"s-eff","channel_or_purpose":"email","value":"` + value + `","source":"UI"`
	if effective != "" {
		b += `,"effective_at":"` + effective + `"`
	}
	return b + `}`
}

func setPref(r http.Handler, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	h := map[string]string{"X-Principal-Id": "p1", "X-Tenant-Id": testTenant, "X-Correlation-ID": "c", "Idempotency-Key": "k-" + body}
	for k, v := range hdr {
		h[k] = v
	}
	w := rawDo(r, http.MethodPost, "/privacy/preferences", body, h)
	return w, nil
}

func getPref(t *testing.T, r http.Handler) map[string]any {
	t.Helper()
	w := doRequest(r, http.MethodGet, "/privacy/preferences?subject_ref=s-eff&channel_or_purpose=email", nil, testTenant)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	return decodeMap(t, w)
}

func TestPreference_EffectiveAt_AcceptedReturnedAndNormalizedToUTC(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	// +05:30 offset with sub-microsecond digits: stored as the same instant in UTC, µs precision.
	w, _ := setPref(r, prefBody("ENABLED", "2026-01-02T10:00:00.1234567+05:30"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	m := decodeMap(t, w)
	if m["effective_at"] != "2026-01-02T04:30:00.123456Z" {
		t.Fatalf("effective_at must be the same instant in UTC at µs precision, got %v", m["effective_at"])
	}
	if m["created_at"] == nil || m["effective_at"] == m["created_at"] {
		t.Fatalf("effective_at must be separate from created_at: %v", m)
	}
	got := getPref(t, r)
	if got["effective_at"] != "2026-01-02T04:30:00.123456Z" || got["value"] != "ENABLED" {
		t.Fatalf("read must return the governing assertion with its effective time: %v", got)
	}
}

func TestPreference_EffectiveAt_DefaultsToTheRecordingInstant(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	before := time.Now().UTC().Add(-2 * time.Second)
	w, _ := setPref(r, prefBody("DISABLED", ""), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d", w.Code)
	}
	m := decodeMap(t, w)
	eff, err := time.Parse(time.RFC3339Nano, m["effective_at"].(string))
	if err != nil || eff.Before(before) || eff.After(time.Now().UTC().Add(2*time.Second)) {
		t.Fatalf("default effective_at must be the recording instant, got %v (%v)", m["effective_at"], err)
	}
	if m["effective_at"] != m["created_at"] {
		t.Fatalf("by default the effective time equals the recording time: %v", m)
	}
}

func TestPreference_EffectiveAt_InvalidValuesAreRejected(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	for _, bad := range []string{`"not-a-time"`, `"2026-01-02"`, `"2026-01-02T10:00:00"`, `12345`, `"0001-01-01T00:00:00Z"`} {
		body := `{"subject_ref":"s","channel_or_purpose":"email","value":"ENABLED","source":"UI","effective_at":` + bad + `}`
		w, _ := setPref(r, body, map[string]string{"Idempotency-Key": "k-bad-" + bad})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("effective_at %s must be 400, got %d %s", bad, w.Code, w.Body.String())
		}
	}
}

func TestPreference_Resolution_UsesEffectiveTimeNotRecordingOrder(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, d).Format(time.RFC3339) }
	// Recorded first, effective yesterday.
	setPref(r, prefBody("ENABLED", day(-1)), nil)
	// Recorded second, but takes effect LATER than the first -> governs.
	setPref(r, prefBody("DISABLED", day(-0)), nil)
	// Recorded last, but with an EARLIER effective time (a backdated capture): must NOT override.
	setPref(r, prefBody("UNSET", day(-10)), nil)
	if got := getPref(t, r); got["value"] != "DISABLED" {
		t.Fatalf("the assertion with the latest effective time governs, got %v", got["value"])
	}
}

func TestPreference_Resolution_FutureAssertionDoesNotGovernYet(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	setPref(r, prefBody("ENABLED", time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)), nil)
	setPref(r, prefBody("DISABLED", time.Now().UTC().AddDate(0, 0, 7).Format(time.RFC3339)), nil)
	if got := getPref(t, r); got["value"] != "ENABLED" {
		t.Fatalf("a future-dated assertion is not in force yet, got %v", got["value"])
	}
	// With only a future-dated assertion nothing is in force.
	r2 := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	setPref(r2, prefBody("ENABLED", time.Now().UTC().AddDate(0, 0, 3).Format(time.RFC3339)), nil)
	if got := getPref(t, r2); got["value"] != string(domain.PreferenceNotApplicable) {
		t.Fatalf("no assertion in force must resolve NOT_APPLICABLE, got %v", got["value"])
	}
}

func TestPreference_EffectiveAt_ParticipatesInIdempotency(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubAuthz{}, grantingPurposeRegistry())
	hdr := map[string]string{"Idempotency-Key": "k-eff-idem"}
	w1, _ := setPref(r, prefBody("ENABLED", "2026-01-02T10:00:00Z"), hdr)
	w2, _ := setPref(r, prefBody("ENABLED", "2026-01-02T10:00:00Z"), hdr)
	if w1.Code != http.StatusCreated || w2.Header().Get("Idempotency-Replay") != "true" || w1.Body.String() != w2.Body.String() || len(st.preferences) != 1 {
		t.Fatalf("identical request must replay: %d replay=%q prefs=%d", w2.Code, w2.Header().Get("Idempotency-Replay"), len(st.preferences))
	}
	w3, _ := setPref(r, prefBody("ENABLED", "2026-01-03T10:00:00Z"), hdr)
	if w3.Code != http.StatusConflict || !strings.Contains(w3.Body.String(), domain.ErrCodeIdempotencyMismatch) {
		t.Fatalf("a different effective_at under the same key must be 409, got %d %s", w3.Code, w3.Body.String())
	}
}

func TestPreference_ExistingBehaviourUnchanged_NeverImpliesConsent(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubAuthz{}, grantingPurposeRegistry())
	setPref(r, prefBody("ENABLED", "2026-01-02T10:00:00Z"), nil)
	w := doRequest(r, http.MethodGet, "/privacy/consents?subject_ref=s-eff&purpose_id="+testPurpose, nil, testTenant)
	if m := decodeMap(t, w); m["status"] != string(domain.ConsentStatusNotRequested) {
		t.Fatalf("a preference must never become consent: %v", m)
	}
}
