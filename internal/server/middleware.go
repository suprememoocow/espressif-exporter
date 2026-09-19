package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// recoverMiddleware keeps one malformed device response from taking down the exporter
// for the other ninety-nine.
func recoverMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Error("panic serving request",
					"path", r.URL.Path,
					"panic", v,
					"stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// limitInFlight bounds concurrent handler goroutines independently of the probe
// semaphore, so a burst of requests cannot exhaust memory before probing even starts.
func limitInFlight(n int, next http.Handler) http.Handler {
	sem := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "too many concurrent requests", http.StatusServiceUnavailable)
		}
	})
}
