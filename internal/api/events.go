package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// defaultEventsHeartbeat is the interval between SSE comment lines (docs/14).
	defaultEventsHeartbeat = 25 * time.Second
	// eventsWriteTimeout bounds one write to a client that has stopped reading.
	eventsWriteTimeout = 10 * time.Second
)

// handleJobEvents streams job status transitions as server-sent events. Nothing is
// replayed: only transitions committed after the subscription are sent. The stream ends
// when the client disconnects, falls too far behind, or the server shuts down.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	events, cancel := s.store.SubscribeJobEvents()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if r.Method == http.MethodHead {
		return
	}

	rc := http.NewResponseController(w)
	send := func(frame string) bool {
		// Deadlines are unsupported by some ResponseWriters; the stream still works.
		_ = rc.SetWriteDeadline(time.Now().Add(eventsWriteTimeout))
		if _, err := io.WriteString(w, frame); err != nil {
			return false
		}
		if err := rc.Flush(); err != nil {
			return false
		}
		_ = rc.SetWriteDeadline(time.Time{})
		return true
	}

	// Sent after subscribing, so a client that has seen it misses no later transition.
	if !send(": connected\n\n") {
		return
	}

	heartbeat := time.NewTicker(s.eventsHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !send(": heartbeat\n\n") {
				return
			}
		case ev, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(JobStatusEventDTO{
				JobID:      ev.JobID,
				Title:      ev.Title,
				Status:     string(ev.Status),
				PrevStatus: string(ev.PrevStatus),
				Agent:      ev.Agent,
				Host:       ev.Host,
				UpdatedAt:  ev.UpdatedAt,
			})
			if err != nil {
				s.logger.Printf("WARNING: encode job event for job %d: %v", ev.JobID, err)
				continue
			}
			if !send(fmt.Sprintf("event: job_status\ndata: %s\n\n", data)) {
				return
			}
		}
	}
}
