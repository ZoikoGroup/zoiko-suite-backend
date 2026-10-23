// Package currency reads currency reference data from currency-registry-svc (REF-02).
package currency

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Client fetches a currency's minor unit and caches answers briefly. It fails closed: an unreachable registry is an
// error, never a guess.
type Client struct {
	base string
	http *http.Client
	ttl  time.Duration
	mu   sync.Mutex
	c    map[string]cached
}

type cached struct {
	units int
	until time.Time
}

// New builds a client for the registry at base.
func New(base string) *Client {
	return &Client{base: base, http: &http.Client{Timeout: 2 * time.Second}, ttl: 5 * time.Minute, c: map[string]cached{}}
}

// MinorUnits returns the minor-unit decimals of a currency in force at asOf.
func (c *Client) MinorUnits(ctx context.Context, code string, asOf time.Time) (int, error) {
	key := code + "|" + asOf.UTC().Format("2006-01-02")
	c.mu.Lock()
	if e, ok := c.c[key]; ok && time.Now().Before(e.until) {
		c.mu.Unlock()
		return e.units, nil
	}
	c.mu.Unlock()
	u := fmt.Sprintf("%s/v1/currencies/%s?as_of=%s", c.base, url.PathEscape(code), url.QueryEscape(asOf.UTC().Format(time.RFC3339)))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("currency registry answered %d for %s", resp.StatusCode, code)
	}
	var body struct {
		MinorUnit *struct {
			MinorUnit int `json:"minor_unit"`
		} `json:"minor_unit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	if body.MinorUnit == nil || body.MinorUnit.MinorUnit < 0 || body.MinorUnit.MinorUnit > 6 {
		return 0, fmt.Errorf("currency registry has no minor unit for %s", code)
	}
	c.mu.Lock()
	c.c[key] = cached{body.MinorUnit.MinorUnit, time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return body.MinorUnit.MinorUnit, nil
}
