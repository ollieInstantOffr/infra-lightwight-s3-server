package db

import (
	"context"
	"testing"
	"time"
)

// The log query is built by string concatenation, which is where placeholder
// numbering goes wrong — and a mismatch is a runtime error, not a compile one.
// Every filter is exercised against a real database so a malformed query fails
// here rather than the first time someone opens the log viewer.
func TestListRequestLogsAcceptsEveryFilter(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := InsertRequestLogs(ctx, pool, []RequestLog{{
		At: time.Now(), RequestID: "ABC123", Method: "GET", Bucket: "b",
		Key: "some/key.txt", Path: "/b/some/key.txt", Status: 403,
		ErrorCode: "SignatureDoesNotMatch", Reason: "credential scope date mismatch",
		AccessKeyID: "AKIATEST", ClientIP: "10.0.0.1", DurationMS: 12,
	}}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	filters := map[string]LogFilter{
		"empty":         {},
		"since":         {Since: time.Now().Add(-time.Hour)},
		"until":         {Until: time.Now().Add(time.Hour)},
		"surface":       {Surface: "s3"},
		"only errors":   {OnlyErrors: true},
		"status range":  {StatusFrom: 400, StatusTo: 499},
		"error code":    {ErrorCode: "SignatureDoesNotMatch"},
		"bucket":        {Bucket: "b"},
		"key prefix":    {KeyPrefix: "some/"},
		"method":        {Method: "GET"},
		"access key":    {AccessKeyID: "AKIATEST"},
		"search key":    {Search: "some/key"},
		"search reason": {Search: "scope date"},
		"search reqid":  {Search: "ABC123"},
		"before":         {Before: 999999},
		"min duration":   {MinDurationMS: 10},
		"everything": {
			Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour),
			Surface: "s3", OnlyErrors: true, StatusFrom: 400, StatusTo: 499,
			ErrorCode: "SignatureDoesNotMatch", Bucket: "b", KeyPrefix: "some/",
			Method: "GET", AccessKeyID: "AKIATEST", Search: "scope", Before: 999999,
			MinDurationMS: 10,
		},
	}

	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			if _, err := ListRequestLogs(ctx, pool, filter); err != nil {
				t.Fatalf("filter %q produced an invalid query: %v", name, err)
			}
		})
	}
}

// Each of the three search targets must actually match.
func TestSearchMatchesKeyReasonAndRequestID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := InsertRequestLogs(ctx, pool, []RequestLog{{
		At: time.Now(), RequestID: "FINDME99", Method: "PUT", Bucket: "b",
		Key: "reports/quarterly.pdf", Status: 500, Reason: "disk full while writing",
	}}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	for name, search := range map[string]string{
		"by key":        "quarterly",
		"by reason":     "disk full",
		"by request id": "FINDME99",
	} {
		t.Run(name, func(t *testing.T) {
			found, err := ListRequestLogs(ctx, pool, LogFilter{Search: search})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(found) == 0 {
				t.Errorf("searching %q found nothing", search)
			}
		})
	}
}

// Failures and sampled successes expire on different schedules, because a
// sample is worthless after a few days and a failure may be the thing someone
// is looking for a month later.
func TestPurgeKeepsFailuresLongerThanSamples(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := InsertRequestLogs(ctx, pool, []RequestLog{
		{At: old, Status: 200, Sampled: true},
		{At: old, Status: 500, Sampled: false},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	// Samples expire after 7 days, failures after 30.
	if _, err := PurgeLogs(ctx, pool, 30*24*time.Hour, 7*24*time.Hour, 30*24*time.Hour); err != nil {
		t.Fatalf("PurgeLogs: %v", err)
	}

	remaining, err := ListRequestLogs(ctx, pool, LogFilter{})
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("got %d rows after purge, want 1", len(remaining))
	}
	if remaining[0].Status != 500 {
		t.Errorf("purge kept the sample and dropped the failure")
	}
}

func TestServerEventsRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := InsertServerEvents(ctx, pool, []ServerEvent{{
		At: time.Now(), Level: "ERROR", Message: "could not send sign-in email",
		Attributes: map[string]any{"email": "someone@example.com"},
	}}); err != nil {
		t.Fatalf("InsertServerEvents: %v", err)
	}

	events, err := ListServerEvents(ctx, pool, "ERROR", 0, 10)
	if err != nil {
		t.Fatalf("ListServerEvents: %v", err)
	}
	if len(events) != 1 || events[0].Message != "could not send sign-in email" {
		t.Fatalf("got %+v", events)
	}
	if events[0].Attributes["email"] != "someone@example.com" {
		t.Errorf("attributes did not round-trip: %v", events[0].Attributes)
	}

	// WARN filter must not return the ERROR row.
	warnings, err := ListServerEvents(ctx, pool, "WARN", 0, 10)
	if err != nil {
		t.Fatalf("ListServerEvents: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("level filter returned %d rows for WARN", len(warnings))
	}
}

// The two pieces ILS-106 added, checked for correctness rather than just that
// the query parses: MinDurationMS actually excludes the fast requests, and
// Operation round-trips through the CopyFrom insert and back out.
func TestMinDurationMSExcludesFasterRequests(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := InsertRequestLogs(ctx, pool, []RequestLog{
		{At: time.Now(), RequestID: "fast", Method: "GET", Bucket: "b", Path: "/b/k", Status: 200, DurationMS: 50},
		{At: time.Now(), RequestID: "slow", Method: "GET", Bucket: "b", Path: "/b/k", Status: 200, DurationMS: 900},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	entries, err := ListRequestLogs(ctx, pool, LogFilter{MinDurationMS: 500})
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	if len(entries) != 1 || entries[0].RequestID != "slow" {
		t.Fatalf("MinDurationMS=500 returned %+v, want only the 900ms request", entries)
	}
}

func TestOperationRoundTrips(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := InsertRequestLogs(ctx, pool, []RequestLog{
		{At: time.Now(), RequestID: "op-1", Method: "GET", Bucket: "b", Path: "/b/k",
			Operation: "GetObject", Status: 200},
		// A request that never reached routing — a bad signature, say — has no
		// operation, and that has to survive as empty rather than becoming a
		// stray "Unknown" row that would pollute a GROUP BY operation.
		{At: time.Now(), RequestID: "op-2", Method: "GET", Path: "/b/k", Status: 403},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	entries, err := ListRequestLogs(ctx, pool, LogFilter{})
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	byID := map[string]string{}
	for _, e := range entries {
		byID[e.RequestID] = e.Operation
	}
	if byID["op-1"] != "GetObject" {
		t.Errorf("op-1 operation = %q, want GetObject", byID["op-1"])
	}
	if byID["op-2"] != "" {
		t.Errorf("op-2 operation = %q, want empty for a request that never reached routing", byID["op-2"])
	}
}
