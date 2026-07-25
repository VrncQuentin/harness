package ui

import (
	"net/http"

	"github.com/VrncQuentin/harness/internal/metrics"
)

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.prometheusEnabled() {
		http.NotFound(w, r)
		return
	}
	store := s.getMetricsStore()
	if store == nil {
		http.Error(w, "metrics store unavailable", http.StatusServiceUnavailable)
		return
	}
	points, err := store.Latest()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", metrics.PrometheusContentType)
	_, _ = w.Write([]byte(metrics.PrometheusText(points)))
}

func (s *Server) prometheusEnabled() bool {
	store := s.configStore()
	if store == nil {
		return false
	}
	cfg, saved, err := store.Load()
	if err != nil || !saved || cfg == nil {
		return false
	}
	return cfg.Metrics.PrometheusEnabled
}
