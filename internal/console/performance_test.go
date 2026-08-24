package console

import (
	"net/http"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

func TestPerformanceEndpointReturnsAWorkingWindow(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	hour := time.Now().UTC().Truncate(time.Hour)
	if err := db.FlushMetrics(t.Context(), console.pool, []db.MetricSample{
		{Hour: hour, StatusClass: 2, Requests: 40, BytesIn: 100, BytesOut: 200},
		{Hour: hour, StatusClass: 5, Requests: 2, BytesIn: 0, BytesOut: 0},
	}); err != nil {
		t.Fatalf("FlushMetrics: %v", err)
	}

	status, body := console.do(t, "GET", "/api/performance?range=1h", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %v", status, body)
	}
	if body["requests"].(float64) != 42 {
		t.Errorf("requests = %v, want 42", body["requests"])
	}
	latency, ok := body["latency"].(map[string]any)
	if !ok {
		t.Fatal("no latency object in the response")
	}
	if _, ok := latency["p99Ms"]; !ok {
		t.Error("latency is missing p99Ms")
	}
	if _, ok := body["slowestOperations"]; !ok {
		t.Error("response is missing slowestOperations")
	}
	if _, ok := body["coverage"]; !ok {
		t.Error("response is missing coverage — the client cannot tell a full window from a purged one without it")
	}
}

func TestPerformanceEndpointRejectsAnUnknownRange(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	status, _ := console.do(t, "GET", "/api/performance?range=99y", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestLogsSlowFilterUsesTheConfiguredThreshold(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	if err := db.InsertRequestLogs(t.Context(), console.pool, []db.RequestLog{
		{At: time.Now(), RequestID: "fast", Method: "GET", Bucket: "b", Path: "/b/k",
			Operation: "GetObject", Status: 200, DurationMS: 50, Surface: "s3"},
		{At: time.Now(), RequestID: "slow", Method: "GET", Bucket: "b", Path: "/b/k",
			Operation: "GetObject", Status: 200, DurationMS: 5000, Surface: "s3"},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	status, body := console.do(t, "GET", "/api/logs?slow=1", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %v", status, body)
	}
	entries, _ := body["logs"].([]any)
	if len(entries) != 1 {
		t.Fatalf("got %d entries with slow=1, want exactly the 5000ms one: %v", len(entries), entries)
	}
	first, _ := entries[0].(map[string]any)
	if first["requestId"] != "slow" {
		t.Errorf("got %v, want the slow request", first["requestId"])
	}
}

func TestLogsOperationFilterIsMoreSpecificThanMethod(t *testing.T) {
	console := newConsole(t)
	console.signIn(t, "admin@example.com")

	if err := db.InsertRequestLogs(t.Context(), console.pool, []db.RequestLog{
		{At: time.Now(), RequestID: "get-object", Method: "GET", Bucket: "b", Path: "/b/k",
			Operation: "GetObject", Status: 200, Surface: "s3"},
		{At: time.Now(), RequestID: "list-v2", Method: "GET", Bucket: "b", Path: "/b",
			Operation: "ListObjectsV2", Status: 200, Surface: "s3"},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	status, body := console.do(t, "GET", "/api/logs?operation=ListObjectsV2", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %v", status, body)
	}
	entries, _ := body["logs"].([]any)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want exactly the ListObjectsV2 one (both share the GET method): %v", len(entries), entries)
	}
}
