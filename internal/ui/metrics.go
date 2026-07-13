package ui

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
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

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var body strings.Builder
	seen := make(map[string]bool)
	for _, pt := range points {
		name := prometheusMetricName(pt.Name)
		if !seen[name] {
			_, _ = fmt.Fprintf(&body, "# HELP %s Latest harness sample for %s.\n", name, pt.Name)
			_, _ = fmt.Fprintf(&body, "# TYPE %s gauge\n", name)
			seen[name] = true
		}
		_, _ = fmt.Fprintf(&body, "%s%s %s\n", name, prometheusLabels(pt.Tags), prometheusFloat(pt.Value))
	}
	_, _ = w.Write([]byte(body.String()))
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

func prometheusMetricName(name string) string {
	var b strings.Builder
	b.WriteString("harness_")
	for _, r := range name {
		valid := r == '_' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func prometheusLabels(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=\"%s\"", prometheusLabelName(k), prometheusEscape(tags[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func prometheusLabelName(name string) string {
	var b strings.Builder
	for i, r := range name {
		valid := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9' && i > 0)
		if valid {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "label"
	}
	return b.String()
}

func prometheusEscape(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return value
}

func prometheusFloat(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "0"
	}
	return fmt.Sprintf("%g", v)
}
