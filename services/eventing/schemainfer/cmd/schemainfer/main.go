// Command schemainfer writes draft payload schemas from captured events.
//
//	schemainfer -in samples.jsonl -out contracts/events -prefix com.zoikosuite.accounting.kernel.
//
// Each input line is {"type": "<canonical type>", "data": {...}} (the event
// envelope's type and data). One <type minus com.zoikosuite.>.schema.json is
// written per type that matches -prefix.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zoiko.io/eventing/schemainfer"
)

func main() {
	in := flag.String("in", "", "JSON-lines file of {type, data}")
	out := flag.String("out", "contracts/events", "output directory")
	prefix := flag.String("prefix", "com.zoikosuite.", "only types with this prefix")
	min := flag.Int("min-samples", schemainfer.DefaultMinSamples, "fewest samples from which a field is required")
	svc := flag.String("service", "", "producing service, named in each description")
	skip := flag.String("skip-prefix", "", "skip types with this prefix (already hand-written)")
	onlySvc := flag.String("only-svc", "", "only samples whose svc field equals this")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	byType := map[string][]json.RawMessage{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var line struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
			Svc  string          `json:"svc"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil || line.Type == "" || len(line.Data) == 0 {
			continue
		}
		if *onlySvc != "" && line.Svc != *onlySvc {
			continue
		}
		if !strings.HasPrefix(line.Type, *prefix) || (*skip != "" && strings.HasPrefix(line.Type, *skip)) {
			continue
		}
		byType[line.Type] = append(byType[line.Type], line.Data)
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		desc := "ZS-EVENT-001 payload of " + t
		if *svc != "" {
			desc += " (" + *svc + ", schema 1.0.0)"
		}
		desc += "."
		b, err := schemainfer.Infer(byType[t], schemainfer.Options{MinSamples: *min, Description: desc})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", t, err)
			os.Exit(1)
		}
		name := strings.TrimPrefix(t, "com.zoikosuite.") + ".schema.json"
		if err := os.WriteFile(filepath.Join(*out, name), b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%-72s %d sample(s)\n", t, len(byType[t]))
	}
}
