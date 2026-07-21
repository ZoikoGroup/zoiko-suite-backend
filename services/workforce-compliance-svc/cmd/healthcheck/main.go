package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	targetURL := "http://localhost:8118/healthz"
	if len(os.Args) > 1 && os.Args[1] != "" {
		targetURL = os.Args[1]
	}

	client := http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get(targetURL)
	if err != nil {
		fmt.Printf("Healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Healthcheck returned status code: %d\n", resp.StatusCode)
		os.Exit(1)
	}

	fmt.Println("Healthcheck OK")
}
