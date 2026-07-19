package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const PrometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// PrometheusText encodes latest metric samples using the Prometheus text
// exposition format. It intentionally lives with metric domain types so HTTP
// handlers only choose when to expose metrics, not how to serialize them.
func PrometheusText(points []DataPoint) string {
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
	return body.String()
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
