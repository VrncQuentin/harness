package metrics

import (
	"math"
	"strings"
	"testing"
)

func TestPrometheusTextEncodesLatestSamples(t *testing.T) {
	body := PrometheusText([]DataPoint{
		{Name: "queue-depth", Value: 2},
		{Name: "process_health", Value: 1, Tags: map[string]string{"process": "llama-server"}},
		{Name: "process_health", Value: math.NaN(), Tags: map[string]string{"bad label": "line\nquote\"slash\\"}},
	})
	for _, want := range []string{
		"# HELP harness_queue_depth Latest harness sample for queue-depth.",
		"# TYPE harness_queue_depth gauge",
		"harness_queue_depth 2",
		`harness_process_health{process="llama-server"} 1`,
		`harness_process_health{bad_label="line\nquote\"slash\\"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("PrometheusText missing %q in:\n%s", want, body)
		}
	}
	if strings.Count(body, "# TYPE harness_process_health gauge") != 1 {
		t.Fatalf("process health TYPE emitted wrong number of times:\n%s", body)
	}
}
