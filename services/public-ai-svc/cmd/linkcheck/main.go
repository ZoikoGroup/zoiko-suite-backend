// Command linkcheck verifies every official route and source page a
// knowledge release can link to (Doc 03 §12/§15/§16, KB-018, Doc 04 §22):
// each must answer 2xx over https, and every redirect hop must stay on an
// allowlisted host. Placeholder URLs (.invalid, example hosts) are reported
// as placeholders, never fetched. Run it before promotion and daily.
//
//	go run ./cmd/linkcheck -dir data/public-kb -out linkcheck.json
//
// It exits 1 when any real link is broken or redirects off the allowlist.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zoiko.io/public-ai-svc/internal/knowledge"
)

// Link is one URL to check and where it came from.
type Link struct {
	URL    string `json:"url"`
	Source string `json:"source"` // route:<key> or source:<id>
}

// Result is one link's outcome.
type Result struct {
	Link
	State    string   `json:"state"` // OK, PLACEHOLDER, BROKEN, OFF_ALLOWLIST
	Status   int      `json:"status,omitempty"`
	Hops     []string `json:"hops,omitempty"`
	Problem  string   `json:"problem,omitempty"`
	Duration int64    `json:"duration_ms"`
}

func main() {
	dir := flag.String("dir", "data/public-kb", "release directory (release.json and sources/)")
	out := flag.String("out", "", "write the JSON report here")
	timeout := flag.Duration("timeout", 10*time.Second, "per-link timeout")
	flag.Parse()

	links, allowed, err := Collect(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "linkcheck:", err)
		os.Exit(2)
	}
	results := Check(context.Background(), links, allowed, &http.Client{Timeout: *timeout})
	b, _ := json.MarshalIndent(results, "", "  ")
	if *out != "" {
		_ = os.WriteFile(*out, append(b, '\n'), 0o644)
	}
	counts := map[string]int{}
	for _, r := range results {
		counts[r.State]++
		if r.State != "OK" && r.State != "PLACEHOLDER" {
			fmt.Printf("%-13s %s (%s): %s\n", r.State, r.URL, r.Source, r.Problem)
		}
	}
	fmt.Printf("linkcheck: %d links, %d ok, %d placeholders, %d broken, %d off-allowlist\n",
		len(results), counts["OK"], counts["PLACEHOLDER"], counts["BROKEN"], counts["OFF_ALLOWLIST"])
	if counts["BROKEN"]+counts["OFF_ALLOWLIST"] > 0 {
		os.Exit(1)
	}
}

// Collect reads the routes and source origins of a release directory.
func Collect(dir string) ([]Link, []string, error) {
	var cfg knowledge.IngestConfig
	b, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, nil, fmt.Errorf("release.json: %w", err)
	}
	var links []Link
	for _, r := range cfg.Routes {
		links = append(links, Link{URL: r.URL, Source: "route:" + r.RouteKey})
	}
	mds, _ := filepath.Glob(filepath.Join(dir, "sources", "*.md"))
	for _, p := range mds {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, err
		}
		sf, err := knowledge.ParseMarkdownSource(string(raw))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		links = append(links, Link{URL: sf.Source.OriginURI, Source: "source:" + sf.Source.SourceID})
	}
	jsons, _ := filepath.Glob(filepath.Join(dir, "sources", "*.source.json"))
	for _, p := range jsons {
		var sf knowledge.SourceFile
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(raw, &sf); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		links = append(links, Link{URL: sf.Source.OriginURI, Source: "source:" + sf.Source.SourceID})
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Source < links[j].Source })
	return links, cfg.AllowedHosts, nil
}

var errOffAllowlist = errors.New("redirect leaves the allowlist")

// Check fetches each real link, following redirects only within allowed hosts.
func Check(ctx context.Context, links []Link, allowed []string, base *http.Client) []Result {
	out := make([]Result, 0, len(links))
	for _, l := range links {
		r := Result{Link: l}
		start := time.Now()
		u, err := url.Parse(l.URL)
		switch {
		case err != nil || u.Host == "":
			r.State, r.Problem = "BROKEN", "not a URL"
		case knowledge.IsPlaceholderURL(l.URL):
			r.State = "PLACEHOLDER"
		case u.Scheme != "https":
			r.State, r.Problem = "BROKEN", "not https"
		case !hostAllowed(allowed, u.Hostname()):
			r.State, r.Problem = "OFF_ALLOWLIST", "host "+u.Hostname()+" is not allowlisted"
		default:
			c := *base
			c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				r.Hops = append(r.Hops, req.URL.String())
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != "https" || !hostAllowed(allowed, req.URL.Hostname()) {
					return errOffAllowlist
				}
				return nil
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
			req.Header.Set("User-Agent", "zoikosuite-public-ai-linkcheck/1")
			resp, err := c.Do(req)
			switch {
			case errors.Is(err, errOffAllowlist):
				r.State, r.Problem = "OFF_ALLOWLIST", "redirects to a host outside the allowlist"
			case err != nil:
				r.State, r.Problem = "BROKEN", err.Error()
			default:
				resp.Body.Close()
				r.Status = resp.StatusCode
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					r.State = "OK"
				} else {
					r.State, r.Problem = "BROKEN", resp.Status
				}
			}
		}
		r.Duration = time.Since(start).Milliseconds()
		out = append(out, r)
	}
	return out
}

// hostAllowed matches exactly, like ingest: a subdomain is a different host.
func hostAllowed(allowed []string, host string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, host) {
			return true
		}
	}
	return false
}
