// Command genasyncapi writes one AsyncAPI 3.1.0 document per catalogue domain.
//
//	go run ./catalogue/cmd/genasyncapi -out asyncapi
//
// The documents are generated, never edited: CI (TestAsyncAPIFilesAreCurrent)
// fails when they differ from what the catalogue produces.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"zoiko.io/eventing/catalogue"
)

func main() {
	out := flag.String("out", "asyncapi", "output directory")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	domains, err := catalogue.AsyncAPIDomains()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, d := range domains {
		b, err := catalogue.AsyncAPI(d)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(*out, d+".asyncapi.json"), b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Printf("wrote %d AsyncAPI documents to %s\n", len(domains), *out)
}
