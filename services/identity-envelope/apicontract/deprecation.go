package apicontract

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Deprecation struct {
	At     time.Time
	Sunset time.Time
	Link   string
}

func SetDeprecation(w http.ResponseWriter, d Deprecation) {
	if !d.At.IsZero() {
		w.Header().Set("Deprecation", "@"+strconv.FormatInt(d.At.Unix(), 10))
	}
	if !d.Sunset.IsZero() {
		w.Header().Set("Sunset", d.Sunset.UTC().Format(http.TimeFormat))
	}
	if strings.TrimSpace(d.Link) != "" {
		w.Header().Set("Link", "<"+strings.TrimSpace(d.Link)+`>; rel="deprecation"`)
	}
}
