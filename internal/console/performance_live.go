package console

import (
	"fmt"
	"net/http"
	"time"
)

// handlePerformanceLive streams a snapshot of the last minute once a second —
// throughput, requests, and how many requests are currently open — straight
// from in-memory state. No database work: the same reasoning as the log tail,
// same SSE mechanics (response controller for the flush, write deadline
// cleared so an open-ended stream is not cut short, X-Accel-Buffering off so
// a buffering proxy does not hold it).
func (s *Server) handlePerformanceLive(w http.ResponseWriter, r *http.Request) {
	if s.Live == nil {
		writeError(w, http.StatusNotImplemented, "Live mode is not available on this build.")
		return
	}

	control := http.NewResponseController(w)
	if err := control.SetWriteDeadline(time.Time{}); err != nil {
		s.Log.Warn("could not clear the write deadline for the performance stream", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	w.WriteHeader(http.StatusOK)
	if err := control.Flush(); err != nil {
		s.Log.Error("the performance stream cannot flush; live mode is unavailable", "error", err)
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			control.Flush()

		case <-ticker.C:
			payload, err := jsonBytes(s.performanceLiveSnapshot())
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			control.Flush()
		}
	}
}

// performanceLivePayload is one second of the Live view.
type performanceLivePayload struct {
	Series []struct {
		At       time.Time `json:"at"`
		Requests int64     `json:"requests"`
		BytesIn  int64     `json:"bytesIn"`
		BytesOut int64     `json:"bytesOut"`
	} `json:"series"`
	RequestsThisSecond int64               `json:"requestsThisSecond"`
	PeakRequests       int64               `json:"peakRequests"`
	InFlightCount      int                 `json:"inFlightCount"`
	InFlight           []inFlightEntryJSON `json:"inFlight"`
}

type inFlightEntryJSON struct {
	Operation string `json:"operation"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	AgeMS     int64  `json:"ageMs"`
}

// maxInFlightListed bounds the per-second payload: a node with hundreds of
// open connections should still send a small, useful list — the oldest ones,
// which are the ones worth looking at — rather than the whole thing every
// second.
const maxInFlightListed = 25

func (s *Server) performanceLiveSnapshot() performanceLivePayload {
	var payload performanceLivePayload

	points := s.Live.Snapshot()
	payload.Series = make([]struct {
		At       time.Time `json:"at"`
		Requests int64     `json:"requests"`
		BytesIn  int64     `json:"bytesIn"`
		BytesOut int64     `json:"bytesOut"`
	}, len(points))
	for i, p := range points {
		payload.Series[i].At = p.At
		payload.Series[i].Requests = p.Requests
		payload.Series[i].BytesIn = p.BytesIn
		payload.Series[i].BytesOut = p.BytesOut
		if p.Requests > payload.PeakRequests {
			payload.PeakRequests = p.Requests
		}
	}
	if len(points) > 0 {
		payload.RequestsThisSecond = points[len(points)-1].Requests
	}

	if s.InFlight != nil {
		entries := s.InFlight.Snapshot()
		payload.InFlightCount = len(entries)
		if len(entries) > maxInFlightListed {
			entries = entries[:maxInFlightListed]
		}
		payload.InFlight = make([]inFlightEntryJSON, len(entries))
		for i, e := range entries {
			payload.InFlight[i] = inFlightEntryJSON{
				Operation: e.Operation, Bucket: e.Bucket, Key: e.Key,
				AgeMS: e.Age.Milliseconds(),
			}
		}
	}

	return payload
}
