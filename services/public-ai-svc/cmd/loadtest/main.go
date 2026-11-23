// Command loadtest drives the public assistant API with concurrent
// anonymous conversations and reports latency percentiles, outcome counts,
// 429s and server errors (Doc 07 §16, QA-T060/T063). It never needs a
// secret: it creates its own sessions like a browser would.
//
//	go run ./cmd/loadtest -url https://staging.example/public-ai/v1 -origin https://www.example -profile 1x -duration 2m
//
// Profiles scale the approved steady rate (DEC-08 sets the real numbers):
// 1x is -rate, 2x doubles it, burst sends 5x for the first tenth of the run.
// It exits 1 when p95 exceeds -p95-max, the server-error rate exceeds
// -max-5xx, or any request fails at the transport level.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// Questions are public, approved-style questions (no personal data).
var questions = []string{
	"What is ZoikoSuite?", "Which finance modules does ZoikoSuite include?", "How are approvals audited?",
	"Does ZoikoSuite support multiple entities?", "Is there a free trial?", "How is data secured?",
	"Can I book a demo?", "What integrations are available?", "Where is data hosted?", "What does payroll cover?",
}

type Options struct {
	URL, Origin string
	Rate        float64 // turns per second at 1x
	Profile     string  // 1x | 2x | burst
	Duration    time.Duration
	Concurrency int
	Client      *http.Client
}

type Report struct {
	Profile     string         `json:"profile"`
	Requests    int            `json:"requests"`
	Statuses    map[string]int `json:"statuses"` // ANSWERED, ABSTAIN, ... or http_<code>
	RateLimited int            `json:"rate_limited"`
	ServerError int            `json:"server_errors"`
	Transport   int            `json:"transport_errors"`
	P50MS       int64          `json:"p50_ms"`
	P95MS       int64          `json:"p95_ms"`
	P99MS       int64          `json:"p99_ms"`
	MaxMS       int64          `json:"max_ms"`
}

func main() {
	var o Options
	flag.StringVar(&o.URL, "url", "http://127.0.0.1:8001/public-ai/v1", "API base URL")
	flag.StringVar(&o.Origin, "origin", "http://localhost:3000", "an allowed Origin")
	flag.Float64Var(&o.Rate, "rate", 2, "steady turns per second (1x)")
	flag.StringVar(&o.Profile, "profile", "1x", "1x, 2x or burst")
	flag.DurationVar(&o.Duration, "duration", 30*time.Second, "how long to run")
	flag.IntVar(&o.Concurrency, "concurrency", 8, "concurrent conversations")
	out := flag.String("out", "", "write the JSON report here")
	p95Max := flag.Duration("p95-max", 8*time.Second, "fail when p95 exceeds this (Doc 07 §10 proposes 8 s end to end)")
	max5xx := flag.Float64("max-5xx", 0.001, "fail when more than this share of requests get a 5xx")
	flag.Parse()

	rep := Run(context.Background(), o)
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	if *out != "" {
		_ = os.WriteFile(*out, append(b, '\n'), 0o644)
	}
	if code := Verdict(rep, *p95Max, *max5xx); code != "" {
		fmt.Fprintln(os.Stderr, "loadtest: FAIL:", code)
		os.Exit(1)
	}
}

// Verdict returns why the run failed, or "".
func Verdict(r Report, p95Max time.Duration, max5xx float64) string {
	switch {
	case r.Requests == 0:
		return "no requests completed"
	case r.Transport > 0:
		return fmt.Sprintf("%d transport errors", r.Transport)
	case float64(r.ServerError)/float64(r.Requests) > max5xx:
		return fmt.Sprintf("%d of %d requests got a 5xx", r.ServerError, r.Requests)
	case time.Duration(r.P95MS)*time.Millisecond > p95Max:
		return fmt.Sprintf("p95 %d ms exceeds %s", r.P95MS, p95Max)
	}
	return ""
}

// Run sends turns at the profile's rate from Concurrency conversations.
func Run(ctx context.Context, o Options) Report {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 1
	}
	ctx, cancel := context.WithTimeout(ctx, o.Duration)
	defer cancel()
	ticks := make(chan struct{})
	go pace(ctx, o, ticks)

	var mu sync.Mutex
	rep := Report{Profile: o.Profile, Statuses: map[string]int{}}
	var lat []time.Duration
	var wg sync.WaitGroup
	for w := 0; w < o.Concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := &conv{o: o}
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ticks:
					if !ok {
						return
					}
				}
				start := time.Now()
				status, code, err := c.turn(ctx, questions[(w+i)%len(questions)])
				d := time.Since(start)
				if ctx.Err() != nil && err != nil {
					return // cut off by the end of the run, not a failure
				}
				mu.Lock()
				rep.Requests++
				switch {
				case err != nil:
					rep.Transport++
				case code == http.StatusTooManyRequests:
					rep.RateLimited++
					rep.Statuses["http_429"]++
				case code >= 500:
					rep.ServerError++
					rep.Statuses[fmt.Sprintf("http_%d", code)]++
				case code != http.StatusOK:
					rep.Statuses[fmt.Sprintf("http_%d", code)]++
				default:
					rep.Statuses[status]++
					lat = append(lat, d)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) int64 {
		if len(lat) == 0 {
			return 0
		}
		i := int(p*float64(len(lat))+0.5) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(lat) {
			i = len(lat) - 1
		}
		return lat[i].Milliseconds()
	}
	rep.P50MS, rep.P95MS, rep.P99MS = pct(0.50), pct(0.95), pct(0.99)
	if len(lat) > 0 {
		rep.MaxMS = lat[len(lat)-1].Milliseconds()
	}
	return rep
}

// pace emits one tick per turn at the profile's rate.
func pace(ctx context.Context, o Options, ticks chan<- struct{}) {
	defer close(ticks)
	rate := o.Rate
	if o.Profile == "2x" {
		rate *= 2
	}
	start := time.Now()
	for {
		r := rate
		if o.Profile == "burst" && time.Since(start) < o.Duration/10 {
			r = rate * 5
		}
		if r <= 0 {
			r = 1
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(float64(time.Second) / r)):
		}
		select {
		case ticks <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
}

// conv is one anonymous conversation; it renews its session on 401.
type conv struct {
	o            Options
	id, grant    string
	sessionTries int
}

func (c *conv) turn(ctx context.Context, msg string) (string, int, error) {
	if c.grant == "" {
		if err := c.session(ctx); err != nil {
			return "", 0, err
		}
	}
	tid := uuid()
	body, _ := json.Marshal(map[string]string{"schema_version": "1.0", "session_id": c.id, "client_turn_id": tid, "message": msg})
	code, b, err := c.post(ctx, "/turns", body, map[string]string{"X-Public-Chat-Session": c.grant, "Idempotency-Key": tid})
	if err != nil {
		return "", 0, err
	}
	if code == http.StatusUnauthorized {
		c.grant = "" // expired: the next turn starts a new session
	}
	var env struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(b, &env)
	return env.Status, code, nil
}

func (c *conv) session(ctx context.Context) error {
	code, b, err := c.post(ctx, "/sessions", []byte(`{"schema_version":"1.0","locale":"en-US"}`), map[string]string{"Idempotency-Key": uuid()})
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return nil // counted by the caller as a failed turn (no grant)
	}
	var s struct {
		SessionID    string `json:"session_id"`
		SessionGrant string `json:"session_grant"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	c.id, c.grant = s.SessionID, s.SessionGrant
	return nil
}

func (c *conv) post(ctx context.Context, path string, body []byte, hdr map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.o.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.o.Origin)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.o.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, b, err
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
