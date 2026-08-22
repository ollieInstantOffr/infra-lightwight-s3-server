package metrics

import (
	"testing"
	"time"
)

func TestRecordGroupsByHourAndStatusClass(t *testing.T) {
	collector := New()
	fixed := time.Date(2026, 8, 22, 14, 30, 0, 0, time.UTC)
	collector.now = func() time.Time { return fixed }

	collector.Record(200, 10, 100)
	collector.Record(204, 5, 0)
	collector.Record(404, 0, 20)
	collector.Record(500, 0, 0)

	samples := collector.drain()
	if len(samples) != 3 {
		t.Fatalf("got %d rollup cells, want 3 (2xx, 4xx, 5xx)", len(samples))
	}

	byClass := map[int]int64{}
	for _, sample := range samples {
		byClass[sample.StatusClass] += sample.Requests
		if !sample.Hour.Equal(fixed.Truncate(time.Hour)) {
			t.Errorf("sample hour = %s, want it truncated to %s", sample.Hour, fixed.Truncate(time.Hour))
		}
	}
	if byClass[2] != 2 || byClass[4] != 1 || byClass[5] != 1 {
		t.Errorf("counts by class = %v, want 2xx:2 4xx:1 5xx:1", byClass)
	}
}

func TestDrainEmptiesTheCollector(t *testing.T) {
	collector := New()
	collector.Record(200, 0, 0)

	if len(collector.drain()) != 1 {
		t.Fatal("first drain returned nothing")
	}
	if len(collector.drain()) != 0 {
		t.Error("second drain returned counts that were already taken")
	}
}

// A failed flush must not discard the counts it was carrying.
func TestRestorePutsCountsBack(t *testing.T) {
	collector := New()
	collector.Record(200, 1, 2)
	collector.Record(200, 1, 2)

	samples := collector.drain()
	collector.restore(samples)

	restored := collector.drain()
	if len(restored) != 1 || restored[0].Requests != 2 {
		t.Fatalf("restored = %+v, want a single cell with 2 requests", restored)
	}
	if restored[0].BytesIn != 2 || restored[0].BytesOut != 4 {
		t.Errorf("restored bytes = in %d out %d, want in 2 out 4", restored[0].BytesIn, restored[0].BytesOut)
	}
}

// Recording happens on every request, so it must be safe from many goroutines.
func TestRecordIsConcurrencySafe(t *testing.T) {
	collector := New()
	const writers, each = 16, 200

	done := make(chan struct{})
	for range writers {
		go func() {
			for range each {
				collector.Record(200, 1, 1)
			}
			done <- struct{}{}
		}()
	}
	for range writers {
		<-done
	}

	samples := collector.drain()
	var total int64
	for _, sample := range samples {
		total += sample.Requests
	}
	if want := int64(writers * each); total != want {
		t.Errorf("recorded %d requests, want %d", total, want)
	}
}

// An out-of-range status must land somewhere rather than being dropped or
// violating the table's check constraint.
func TestRecordClampsUnknownStatus(t *testing.T) {
	collector := New()
	collector.Record(0, 0, 0)
	collector.Record(999, 0, 0)

	for _, sample := range collector.drain() {
		if sample.StatusClass < 1 || sample.StatusClass > 5 {
			t.Errorf("status class %d is outside the permitted range", sample.StatusClass)
		}
	}
}
