// Command healthcheck is a minimal probe binary for use in this
// service's Dockerfile HEALTHCHECK instruction.
//
// distroless/static-debian12 has no shell, no curl, no wget — the usual
// tools a HEALTHCHECK directive would invoke don't exist in that image.
// This compiles to a tiny standalone binary instead: GET /healthz on
// localhost, exit 0 on 200, exit 1 on anything else (including a
// connection failure). context.md §7.8: this is the first service in
// this repo to use this pattern, not a copy of an existing one —
// verified directly that no other service here has a cmd/healthcheck
// binary or a Dockerfile HEALTHCHECK instruction yet.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	port := 8087
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}

	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: request failed:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unhealthy status", resp.StatusCode)
		os.Exit(1)
	}
	os.Exit(0)
}
