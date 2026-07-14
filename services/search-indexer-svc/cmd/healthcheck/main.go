// cmd/healthcheck is a minimal binary that performs an HTTP GET and exits 0
// on 200, 1 otherwise. Used by Docker and Kubernetes probes.
// Identical to the healthcheck pattern in obligations-svc.
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <url>")
		os.Exit(1)
	}
	resp, err := http.Get(os.Args[1]) //nolint:gosec,noctx
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: GET %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		os.Exit(1)
	}
}
