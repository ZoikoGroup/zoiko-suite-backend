package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8122"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	// readyz, not healthz. Liveness answers 200 whenever the process is up, so a
	// dead database pool reads as healthy while every read returns 500 — the
	// shape that made three services in this stack look healthy while broken.
	// This service's compose entry runs this binary rather than naming a URL, so
	// the path lives here rather than in docker-compose.yml.
	resp, err := client.Get(fmt.Sprintf("http://localhost:%s/readyz", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck returned %d\n", resp.StatusCode)
		os.Exit(1)
	}
	fmt.Println("healthy")
}
