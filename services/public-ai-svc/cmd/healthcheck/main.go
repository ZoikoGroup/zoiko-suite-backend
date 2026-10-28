// Command healthcheck is the container health probe: it exits 0 only when
// /health/live answers 200.
package main

import (
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := os.Getenv("PUBLIC_AI_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/health/live")
	if err != nil {
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
