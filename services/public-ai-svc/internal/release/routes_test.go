package release

import "testing"

// CT-039: a route key the release does not publish, a route outside its
// effective window, or one whose gating claim is not verified resolves to
// nothing, so the pipeline hides the CTA instead of redirecting anywhere.
func TestRouteResolutionFailsClosed(t *testing.T) {
	snap, err := setup(t).rt.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"no-such-route", "", "https://evil.example", "../admin"} {
		if r, ok := snap.Route(key, "US", now); ok {
			t.Errorf("route %q resolved to %+v", key, r)
		}
	}
	if len(snap.Routes) == 0 {
		t.Fatal("fixture release has no routes")
	}
	r := snap.Routes[0]
	if _, ok := snap.Route(r.RouteKey, "US", r.EffectiveUntil); ok {
		t.Errorf("route %s resolved after its effective window", r.RouteKey)
	}
	if _, ok := snap.Route(r.RouteKey, "US", r.EffectiveFrom.Add(-1)); ok {
		t.Errorf("route %s resolved before its effective window", r.RouteKey)
	}
}
