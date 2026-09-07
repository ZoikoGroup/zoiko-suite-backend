package main

import (
	"testing"

	"zoiko.io/identity-context-svc/internal/config"
)

// The failure this guards against is silent: a managed endpoint with TLS off
// does not reject the connection, it stops answering, so the only symptom is a
// timeout on the startup Ping five seconds later.
func TestRedisOptions(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.RedisConfig
		wantAddr string
		wantPass string
		wantTLS  bool
		wantErr  bool
	}{
		{
			name:     "local container: no password, no TLS",
			cfg:      config.RedisConfig{Host: "redis", Port: 6379},
			wantAddr: "redis:6379",
		},
		{
			name:     "upstash rediss:// URL carries credentials and TLS",
			cfg:      config.RedisConfig{URL: "rediss://default:AaBbCc123XyZ@apt-cat-1234.upstash.io:6379"},
			wantAddr: "apt-cat-1234.upstash.io:6379",
			wantPass: "AaBbCc123XyZ",
			wantTLS:  true,
		},
		{
			// redis:// (one s) is accepted and simply leaves TLS off. It is a
			// valid URL, so nothing rejects it — the endpoint stops answering
			// instead and the Ping times out. Pinned because the parse layer
			// cannot be the place this is caught.
			name:     "redis:// parses but leaves TLS off",
			cfg:      config.RedisConfig{URL: "redis://default:AaBbCc123XyZ@apt-cat-1234.upstash.io:6379"},
			wantAddr: "apt-cat-1234.upstash.io:6379",
			wantPass: "AaBbCc123XyZ",
			wantTLS:  false,
		},
		{
			// A password holding a character that must be percent-encoded in
			// userinfo cannot go in the URL raw. Upstash tokens are
			// alphanumeric so this does not bite there; the discrete
			// REDIS_PASSWORD form is the way out when a provider's is not.
			name:    "unencoded special character in the password is rejected",
			cfg:     config.RedisConfig{URL: "rediss://default:has a space@apt-cat-1234.upstash.io:6379"},
			wantErr: true,
		},
		{
			name:     "percent-encoded password is decoded",
			cfg:      config.RedisConfig{URL: "rediss://default:has%20a%20space@apt-cat-1234.upstash.io:6379"},
			wantAddr: "apt-cat-1234.upstash.io:6379",
			wantPass: "has a space",
			wantTLS:  true,
		},
		{
			name:     "URL wins outright over Host/Port/Password/TLS",
			cfg:      config.RedisConfig{URL: "rediss://default:fromurl@managed:6379", Host: "redis", Port: 6379, Password: "fromparts", TLSEnabled: false},
			wantAddr: "managed:6379",
			wantPass: "fromurl",
			wantTLS:  true,
		},
		{
			name:     "discrete form with TLS on",
			cfg:      config.RedisConfig{Host: "apt-cat-1234.upstash.io", Port: 6379, Password: "tok", TLSEnabled: true},
			wantAddr: "apt-cat-1234.upstash.io:6379",
			wantPass: "tok",
			wantTLS:  true,
		},
		{
			name:    "the REST endpoint is not a Redis URL",
			cfg:     config.RedisConfig{URL: "https://apt-cat-1234.upstash.io"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := redisOptions(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opts.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", opts.Addr, tt.wantAddr)
			}
			if opts.Password != tt.wantPass {
				t.Errorf("Password = %q, want %q", opts.Password, tt.wantPass)
			}
			if gotTLS := opts.TLSConfig != nil; gotTLS != tt.wantTLS {
				t.Errorf("TLS = %v, want %v", gotTLS, tt.wantTLS)
			}
		})
	}
}

// ParseURL puts the input in its error text. Nothing that reaches a log may
// contain the password.
func TestRedisOptionsErrorOmitsSecret(t *testing.T) {
	_, err := redisOptions(config.RedisConfig{URL: "https://default:sup3rs3cret@apt-cat-1234.upstash.io"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); contains(got, "sup3rs3cret") {
		t.Errorf("error text leaks the password: %q", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
