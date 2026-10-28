package apicontract

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:\-]{8,128}$`)

type requestIDKey struct{}

func ValidRequestID(id string) bool {
	return validRequestID.MatchString(strings.TrimSpace(id))
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if !ValidRequestID(id) {
			id = strings.TrimSpace(r.Header.Get(HeaderLegacyRequestID))
		}
		if !ValidRequestID(id) {
			id = NewRequestID()
		}
		r.Header.Set(HeaderRequestID, id)
		r.Header.Set(HeaderLegacyRequestID, id)
		w.Header().Set(HeaderRequestID, id)
		w.Header().Set(HeaderLegacyRequestID, id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

func RequestIDFrom(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDKey{}).(string); ok && id != "" {
		return id
	}
	return firstNonEmpty(r.Header.Get(HeaderRequestID), r.Header.Get(HeaderLegacyRequestID))
}

func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func NewRequestID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("req_%012x%x", time.Now().UnixMilli(), b)
}
