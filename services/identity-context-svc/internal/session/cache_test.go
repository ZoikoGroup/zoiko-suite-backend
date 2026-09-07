package session

import "testing"

// The key helpers are the whole storage contract: an envelope written under one
// prefix and read under another is a session that silently never resolves, and
// a reverse-index key that does not match the one revocation reads is a
// revocation that finds nothing. Both fail as "not found" rather than as an
// error, so nothing else in the service would report them.
//
// These assertions are deliberately literal rather than reusing the helpers to
// build the expectation — a test that calls the same function it is checking
// passes for any implementation, including one that returns the wrong prefix or
// calls itself.
func TestKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"session jwt", sessionJWTKey("01ABC"), "session:jwt:01ABC"},
		{"session ctx", sessionCtxKey("01ABC"), "session:ctx:01ABC"},
		{"principal index", principalSessionsKey("p-42"), "principal:sessions:p-42"},
		{"entity index", entitySessionsKey("ent-7"), "entity:sessions:ent-7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
}

// The JWT and context keys must not collide: they hold different values for the
// same session id, and one overwriting the other would serve a JSON record
// where a signed token is expected.
func TestSessionKeysAreDistinct(t *testing.T) {
	if sessionJWTKey("same") == sessionCtxKey("same") {
		t.Fatal("session jwt and ctx keys collide for the same session id")
	}
}
