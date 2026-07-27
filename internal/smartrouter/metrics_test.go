package smartrouter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRouterMetricsRecordSnapshotAndRedaction(t *testing.T) {
	metrics := NewRouterMetrics()
	metrics.Record("public-model", "responses", ExecutionResult{
		RequestID:       "request-safe",
		RouteID:         "secondary",
		UpstreamID:      "pool-secondary",
		StatusCode:      200,
		Success:         true,
		UsageMissing:    true,
		FailureCategory: FailureNone,
		Attempts: []AttemptRecord{
			{
				RouteID:         "primary",
				UpstreamID:      "pool-primary",
				StatusCode:      429,
				FailureCategory: FailureRateLimit,
			},
			{
				RouteID:    "secondary",
				UpstreamID: "pool-secondary",
				StatusCode: 200,
			},
		},
	}, 250*time.Millisecond, 40*time.Millisecond, StreamStateCompleted)

	snapshot := metrics.Snapshot()
	if snapshot.Totals.Requests != 1 ||
		snapshot.Totals.Successes != 1 ||
		snapshot.Totals.Attempts != 2 ||
		snapshot.Totals.Failovers != 1 ||
		snapshot.Totals.UsageMissing != 1 {
		t.Fatalf("metrics totals = %#v", snapshot.Totals)
	}
	if len(snapshot.Requests) != 1 || snapshot.Requests[0].TTFTCount != 1 {
		t.Fatalf("request metrics = %#v", snapshot.Requests)
	}
	if len(snapshot.Routes) != 2 ||
		snapshot.Routes[0].StatusClass != "4xx" ||
		snapshot.Routes[1].StatusClass != "2xx" {
		t.Fatalf("route metrics = %#v", snapshot.Routes)
	}

	rendered, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, secret := range []string{"request-safe", "Authorization", "Bearer", "raw-body", "b64_json"} {
		if strings.Contains(string(rendered), secret) {
			t.Fatalf("metrics leaked %q: %s", secret, rendered)
		}
	}
}

func TestRouterMetricsCountsPartialAndAmbiguousFailures(t *testing.T) {
	metrics := NewRouterMetrics()
	metrics.Record("stream-model", "responses", ExecutionResult{
		FailureCategory: FailureTransient,
	}, time.Second, 100*time.Millisecond, StreamStateFailedPartial)
	metrics.Record("image-model", "images", ExecutionResult{
		FailureCategory: FailureImageAmbiguous,
	}, time.Second, 0, "")

	totals := metrics.Snapshot().Totals
	if totals.Requests != 2 ||
		totals.Failures != 2 ||
		totals.PartialStreamFailures != 1 ||
		totals.ImageAmbiguousFailures != 1 {
		t.Fatalf("metrics totals = %#v", totals)
	}
}
