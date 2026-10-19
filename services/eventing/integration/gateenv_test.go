package integration

import "testing"

func TestGateFromEnv(t *testing.T) {
	t.Setenv("SCHEMA_REGISTRY_URL", "")
	if g, err := GateFromEnv("svc"); g != nil || err != nil {
		t.Fatalf("no registry leaves the gate off: %v %v", g, err)
	}
	t.Setenv("SCHEMA_REGISTRY_URL", "not a url")
	if _, err := GateFromEnv("svc"); err == nil {
		t.Fatal("a malformed registry is a startup error, not an ungated relay")
	}
	t.Setenv("SCHEMA_REGISTRY_URL", "http://registry:8080")
	g, err := GateFromEnv("svc")
	if err != nil || g == nil || !g.AllowUnregistered {
		t.Fatalf("a configured registry builds a lenient gate by default: %v %+v", err, g)
	}
	t.Setenv("SCHEMA_GATE_ALLOW_UNREGISTERED", "false")
	if g, _ = GateFromEnv("svc"); g.AllowUnregistered {
		t.Fatal("false means strict")
	}
}
