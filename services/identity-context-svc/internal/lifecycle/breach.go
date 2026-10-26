package lifecycle

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // HIBP's range API is SHA-1 by protocol; nothing is stored.
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// BreachChecker reports whether a password is known to be compromised.
type BreachChecker interface {
	Breached(ctx context.Context, password string) (bool, error)
}

// commonPasswords are the most-used passwords in public breach corpora, and
// the ones a spray tries first. The local check refuses them outright; a
// deployment adds its own list with PASSWORD_BREACH_LIST_FILE and, where
// outbound calls are permitted, the HIBP range API.
var commonPasswords = []string{
	"123456", "123456789", "12345678", "password", "qwerty123", "qwerty1", "111111", "12345", "secret",
	"123123", "1234567890", "1234567", "000000", "qwerty", "abc123", "password1", "iloveyou", "11111111",
	"dragon", "monkey", "123321", "654321", "666666", "123qwe", "1q2w3e4r", "1qaz2wsx", "qwertyuiop",
	"letmein", "welcome", "admin", "admin123", "passw0rd", "p@ssw0rd", "p@ssword", "password123",
	"password12", "password1234", "welcome1", "welcome123", "changeme", "changeme123", "football",
	"baseball", "sunshine", "princess", "starwars", "whatever", "trustno1", "superman", "michael",
	"shadow", "master", "jennifer", "jordan23", "harley", "hunter2", "ranger", "buster", "soccer",
	"hockey", "killer", "george", "charlie", "andrew", "michelle", "love", "jessica", "pepper",
	"zaq12wsx", "asdfghjkl", "asdf1234", "zxcvbnm", "q1w2e3r4t5", "1q2w3e4r5t", "qazwsx", "987654321",
	"summer2024", "summer2025", "summer2026", "winter2025", "winter2026", "spring2026", "autumn2026",
	"autumn2025", "spring2025", "company123", "welcome2026", "password2026", "password2025",
	"letmein123", "temp1234", "temppassword", "default", "guest", "test1234", "testtest", "zoiko123",
	"zoikosuite", "zoikosuite1", "zoikosuite123", "correcthorsebatterystaple", "qwerty12345",
	"administrator", "rootroot", "toor", "pa55word", "passpass", "access", "login", "master123",
}

// LocalBreach checks an in-memory set (the built-in list plus a file).
type LocalBreach struct{ set map[string]struct{} }

// NewLocalBreach builds the set; extraFile is one password per line (may be "").
func NewLocalBreach(extraFile string) (*LocalBreach, error) {
	b := &LocalBreach{set: map[string]struct{}{}}
	for _, p := range commonPasswords {
		b.set[strings.ToLower(p)] = struct{}{}
	}
	if extraFile == "" {
		return b, nil
	}
	f, err := os.Open(extraFile)
	if err != nil {
		return nil, fmt.Errorf("PASSWORD_BREACH_LIST_FILE: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			b.set[strings.ToLower(line)] = struct{}{}
		}
	}
	return b, sc.Err()
}

// Breached implements BreachChecker.
func (b *LocalBreach) Breached(_ context.Context, password string) (bool, error) {
	_, ok := b.set[strings.ToLower(password)]
	return ok, nil
}

// HIBPBreach queries the Pwned Passwords range API with k-anonymity: only
// the first five hex characters of the SHA-1 leave this process. When the
// API cannot be reached it falls back to the local list rather than refusing
// every password change.
type HIBPBreach struct {
	URL      string
	Client   *http.Client
	Fallback BreachChecker
	Log      *zap.Logger
}

// Breached implements BreachChecker.
func (h *HIBPBreach) Breached(ctx context.Context, password string) (bool, error) {
	if h.Fallback != nil {
		if hit, _ := h.Fallback.Breached(ctx, password); hit {
			return true, nil
		}
	}
	sum := sha1.Sum([]byte(password)) //nolint:gosec
	hexsum := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := hexsum[:5], hexsum[5:]
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL+prefix, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Add-Padding", "true")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		if h.Log != nil {
			h.Log.Warn("breached-password API unreachable — local list only", zap.Error(err))
		}
		return false, nil
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 4<<20))
	for sc.Scan() {
		line := sc.Text()
		suf, count, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(suf), suffix) && strings.TrimSpace(count) != "0" {
			return true, nil
		}
	}
	return false, nil
}
