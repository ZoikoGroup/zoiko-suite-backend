// Command healthcheck is the container HEALTHCHECK. It probes the service's
// own readiness endpoint (/readyz, which pings Postgres) unless a full URL
// is given as the first argument, e.g. "/app/healthcheck http://localhost:8149/healthz".
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	url := ""
	if len(os.Args) > 1 && os.Args[1] != "" {
		url = os.Args[1]
	} else {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8149"
		}
		url = "http://localhost:" + port + "/readyz"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck %s returned %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
}
