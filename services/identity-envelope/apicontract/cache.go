package apicontract

import "net/http"

const DefaultCacheControl = "private, no-store"

func DefaultCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", DefaultCacheControl)
		}
		next.ServeHTTP(w, r)
	})
}

func SetNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", DefaultCacheControl)
}

func SetPublicCache(w http.ResponseWriter, value string) {
	w.Header().Set("Cache-Control", value)
}
