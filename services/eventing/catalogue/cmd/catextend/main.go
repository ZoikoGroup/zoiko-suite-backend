// Command catextend adds event types a service really emits, but section 19
// does not list, to catalogue.json as origin=extension entries.
//
//	catextend -catalogue catalogue/catalogue.json -in samples.jsonl
//
// Each input line is {"type": ..., "svc": ..., "data": ...} (the svc field names
// the emitting service). Types already catalogued are left alone. New entries
// get the service as producer and an honest "not yet declared" consumers note:
// the tool records that a fact exists and who emits it, never who consumes it.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

type entry struct {
	Type      string `json:"type"`
	Family    string `json:"family"`
	Producer  string `json:"producer"`
	Subject   string `json:"subject"`
	Consumers string `json:"consumers"`
	Origin    string `json:"origin,omitempty"`
}

func main() {
	path := flag.String("catalogue", "catalogue/catalogue.json", "catalogue.json to extend")
	in := flag.String("in", "", "JSON-lines samples with type and svc")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		fmt.Fprintln(os.Stderr, "catalogue:", err)
		os.Exit(1)
	}
	have := map[string]bool{}
	for _, e := range entries {
		have[e.Type] = true
	}

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	producer := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var l struct {
			Type string `json:"type"`
			Svc  string `json:"svc"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || !strings.HasPrefix(l.Type, "com.zoikosuite.") || have[l.Type] {
			continue
		}
		if _, seen := producer[l.Type]; !seen {
			producer[l.Type] = l.Svc
		}
	}
	types := make([]string, 0, len(producer))
	for t := range producer {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		seg := strings.Split(strings.TrimPrefix(t, "com.zoikosuite."), ".")
		if len(seg) < 3 {
			fmt.Fprintf(os.Stderr, "skipping %s: not domain.aggregate.fact\n", t)
			continue
		}
		entries = append(entries, entry{
			Type:      t,
			Family:    title(seg[0]) + " (extension)",
			Producer:  humanize(producer[t]),
			Subject:   title(seg[1]),
			Consumers: "Not yet declared (extension observed in the producer; register consumers per section 23)",
			Origin:    "extension",
		})
	}

	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, e := range entries {
		b, _ := marshal(e)
		buf.WriteString("  ")
		buf.Write(b)
		if i < len(entries)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("]\n")
	if err := os.WriteFile(*path, buf.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("added %d extension entries (catalogue now %d)\n", len(types), len(entries))
}

// marshal writes one entry on one line without HTML escaping, as the file already is.
func marshal(e entry) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func title(s string) string {
	words := strings.Split(s, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// humanize turns "payment-status-svc" into "Payment Status Service".
func humanize(svc string) string {
	if svc == "" {
		return "Unknown producer"
	}
	svc = strings.TrimSuffix(svc, "-svc")
	return title(svc) + " Service"
}
