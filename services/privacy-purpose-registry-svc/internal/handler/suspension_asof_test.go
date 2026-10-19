package handler_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

// Suspension is time-sliced in as_of resolution: the version a read returns
// carries the status it had AT the requested instant, not today's.
func TestAsOf_SuspensionIsTimeSliced_HTTP(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))
	step(t, r, tenantA, v, "suspend", ops, nil, http.StatusOK) // SUSPENDED now (real route)

	// Deterministic timeline on the rows: governs from -3h; suspended [-2h, -1h) then again from -30m.
	now := time.Now().UTC()
	h := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	row := st.activityVersions[v.ActivityVersionID]
	row.EffectiveFrom, row.EffectiveTo, row.ApprovedAt = h(-3*time.Hour), nil, h(-4*time.Hour)
	st.suspensions[v.ActivityVersionID] = []stubSuspension{
		{"SUSPEND", now.Add(-2 * time.Hour)}, {"RESUME", now.Add(-time.Hour)}, {"SUSPEND", now.Add(-30 * time.Minute)},
	}

	q := func(d time.Duration) string { return "as_of=" + url.QueryEscape(now.Add(d).Format(time.RFC3339Nano)) }
	status := func(query string) domain.ActivityVersionStatus {
		t.Helper()
		w := call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID+"?"+query, nil, tenantA, "reader-01")
		wantOK(t, w, http.StatusOK)
		got := decode[domain.ProcessingActivityVersion](t, w)
		if got.ActivityVersionID != v.ActivityVersionID {
			t.Fatalf("wrong version %s", got.ActivityVersionID)
		}
		return got.VersionStatus
	}

	cases := []struct {
		name  string
		query string
		want  domain.ActivityVersionStatus
	}{
		{"(1) active before the suspension", q(-150 * time.Minute), domain.ActivityStatusActive},
		{"(2) suspended at as_of", q(-90 * time.Minute), domain.ActivityStatusSuspended},
		{"(3) after the resume", q(-45 * time.Minute), domain.ActivityStatusActive},
		{"(3) after the second suspension", q(-10 * time.Minute), domain.ActivityStatusSuspended},
		// (5) knowledge time: as known 3h ago nothing had been suspended yet.
		{"(5) known_as_of before any suspension", q(-10*time.Minute) + "&known_as_of=" + url.QueryEscape(now.Add(-150*time.Minute).Format(time.RFC3339Nano)), domain.ActivityStatusActive},
		{"(5) known_as_of during the first suspension", q(-10*time.Minute) + "&known_as_of=" + url.QueryEscape(now.Add(-90*time.Minute).Format(time.RFC3339Nano)), domain.ActivityStatusSuspended},
		// (6) a current read (no as_of) is the stored status, unchanged.
		{"(6) current read", "", domain.ActivityStatusSuspended},
	}
	for _, c := range cases {
		if got := status(c.query); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}

	// (7) invalid as_of is still the documented error, suspended or not.
	for _, bad := range []string{"as_of=yesterday", "as_of=2026-01-01", "known_as_of=soon"} {
		wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+v.ActivityID+"?"+bad, nil, tenantA, "reader-01"), http.StatusBadRequest, domain.ErrCodeInvalidAsOf)
	}
}

// (4) Versions with different effective and suspension windows resolve deterministically.
func TestAsOf_SuspensionIsTimeSliced_VersionsResolveDeterministically(t *testing.T) {
	st := newStubStore()
	r := newRouter(st, &stubAuthz{})
	p := newPurpose(t, r, tenantA)
	v1 := toActive(t, r, tenantA, newDraft(t, r, tenantA, goodActivity(p.PurposeID)))
	v2 := toActive(t, r, tenantA, newDraftVersion(t, r, tenantA, v1, goodActivity(p.PurposeID)))

	now := time.Now().UTC()
	h := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	a, b := st.activityVersions[v1.ActivityVersionID], st.activityVersions[v2.ActivityVersionID]
	a.EffectiveFrom, a.EffectiveTo, a.ApprovedAt = h(-6*time.Hour), h(-2*time.Hour), h(-7*time.Hour)
	b.EffectiveFrom, b.EffectiveTo, b.ApprovedAt = h(-2*time.Hour), nil, h(-3*time.Hour)
	st.suspensions[v1.ActivityVersionID] = []stubSuspension{{"SUSPEND", now.Add(-5 * time.Hour)}, {"RESUME", now.Add(-4 * time.Hour)}}
	st.suspensions[v2.ActivityVersionID] = []stubSuspension{{"SUSPEND", now.Add(-90 * time.Minute)}, {"RESUME", now.Add(-30 * time.Minute)}}

	get := func(d time.Duration) domain.ProcessingActivityVersion {
		t.Helper()
		w := call(r, http.MethodGet, activitiesPath+"/"+v1.ActivityID+"?as_of="+url.QueryEscape(now.Add(d).Format(time.RFC3339Nano)), nil, tenantA, "reader-01")
		wantOK(t, w, http.StatusOK)
		return decode[domain.ProcessingActivityVersion](t, w)
	}
	for _, c := range []struct {
		d      time.Duration
		id     string
		status domain.ActivityVersionStatus
	}{
		{-330 * time.Minute, v1.ActivityVersionID, domain.ActivityStatusActive},
		{-270 * time.Minute, v1.ActivityVersionID, domain.ActivityStatusSuspended},
		{-3 * time.Hour, v1.ActivityVersionID, domain.ActivityStatusActive},
		{-105 * time.Minute, v2.ActivityVersionID, domain.ActivityStatusActive},
		{-time.Hour, v2.ActivityVersionID, domain.ActivityStatusSuspended},
		{-10 * time.Minute, v2.ActivityVersionID, domain.ActivityStatusActive},
	} {
		if got := get(c.d); got.ActivityVersionID != c.id || got.VersionStatus != c.status {
			t.Errorf("as_of now%+v: got %s %s, want %s %s", c.d, got.ActivityVersionID, got.VersionStatus, c.id, c.status)
		}
	}
	// Unchanged behaviours: nothing governed before the first version; the RoPA lists ACTIVE only.
	wantErr(t, call(r, http.MethodGet, activitiesPath+"/"+v1.ActivityID+"?as_of="+url.QueryEscape(now.Add(-8*time.Hour).Format(time.RFC3339Nano)), nil, tenantA, "reader-01"), http.StatusNotFound, domain.ErrCodeActivityInactive)
	if got := decode[struct{ Count int }](t, call(r, http.MethodGet, "/privacy/ropa", nil, tenantA, "r")); got.Count != 1 {
		t.Fatalf("RoPA must list the one ACTIVE version, got %d", got.Count)
	}
}
