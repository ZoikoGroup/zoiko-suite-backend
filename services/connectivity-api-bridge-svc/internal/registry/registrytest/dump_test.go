package registrytest

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDump writes the reference contract as request bodies, for live runs against a real deployment. It does
// nothing unless REGISTRYTEST_DUMP names a directory.
func TestDump(t *testing.T) {
	dir := os.Getenv("REGISTRYTEST_DUMP")
	if dir == "" {
		t.Skip("REGISTRYTEST_DUMP not set")
	}
	write := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/"+name, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "1.4.0", "1.5.0", "1.6.0", "1.7.0", "2.0.0"} {
		write("version-"+s+".json", Input(s))
	}
	bad := Input("0.1.0")
	bad.Content.Operations[1].MaxPayloadBytes = 0
	write("version-bad.json", bad)
	write("passed.json", json.RawMessage(Passed()))
}
