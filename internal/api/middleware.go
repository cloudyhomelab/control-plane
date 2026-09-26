package api

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/jobs"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := jobs.NewID()[:12]
		w.Header().Set("X-Request-Id", id)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/healthz" {
			slog.Info("request", "id", id, "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
		}
	})
}

func sortedActionNames(c *config.Config) []string {
	names := make([]string, 0, len(c.Actions))
	for n := range c.Actions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
