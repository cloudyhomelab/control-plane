package api

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/cloudyhomelab/control-plane/internal/config"
	"github.com/cloudyhomelab/control-plane/internal/jobs"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusWriter) WriteHeader(code int) {
	writer.status = code
	writer.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id := jobs.NewID()[:12]
		writer.Header().Set("X-Request-Id", id)
		recorder := &statusWriter{ResponseWriter: writer, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, request)
		if request.URL.Path != "/healthz" {
			slog.Info("request", "id", id, "method", request.Method, "path", request.URL.Path, "status", recorder.status, "dur", time.Since(start))
		}
	})
}

func sortedActionNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Actions))
	for name := range cfg.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
