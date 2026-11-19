package personalagentapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agentflow/internal/domain"
)

const (
	personalAgentHeartbeatInterval = 15 * time.Second
	personalAgentStreamWriteLimit  = 15 * time.Second
)

func personalAgentWantsStream(r *http.Request) bool {
	for _, value := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || mediaType != "text/event-stream" {
			continue
		}
		if raw, ok := params["q"]; ok {
			quality, err := strconv.ParseFloat(raw, 64)
			if err != nil || !(quality > 0 && quality <= 1) {
				continue
			}
		}
		return true
	}
	return false
}

func personalAgentSupportsFlush(w http.ResponseWriter) bool {
	for range 16 {
		if _, ok := w.(interface{ FlushError() error }); ok {
			return true
		}
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = wrapper.Unwrap()
	}
	return false
}

// Check before a POST mutation so an unsupported transport does not create a
// task that its caller could never observe through the requested response.
func personalAgentCheckStreaming(w http.ResponseWriter, r *http.Request) bool {
	if !personalAgentWantsStream(r) || personalAgentSupportsFlush(w) {
		return true
	}
	personalAgentJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming responses are not supported by this transport"})
	return false
}

func (h *personalAgentHandlers) respondTask(w http.ResponseWriter, r *http.Request, task domain.PersonalAgentTask) {
	if personalAgentWantsStream(r) {
		h.stream(w, r, task.ID)
		return
	}
	personalAgentJSON(w, http.StatusAccepted, task)
}

func (h *personalAgentHandlers) events(w http.ResponseWriter, r *http.Request) {
	h.stream(w, r, r.PathValue("id"))
}

// stream attaches to application notifications, never a database polling loop.
// Last-Event-ID is deliberately not used for replay: every connection starts
// from the latest authoritative snapshot and then receives newer revisions.
func (h *personalAgentHandlers) stream(w http.ResponseWriter, r *http.Request, id string) {
	if !personalAgentSupportsFlush(w) {
		personalAgentJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming responses are not supported by this transport"})
		return
	}
	snapshot, updates, cancel, err := h.service.Watch(r.Context(), personalAgentScope(r), id)
	if err != nil {
		personalAgentError(w, err)
		return
	}
	defer cancel()
	first, err := personalAgentStreamEvent("task.snapshot", snapshot)
	if err != nil {
		personalAgentJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot encode task stream"})
		return
	}
	controller := http.NewResponseController(w)
	write := func(payload []byte) bool {
		return personalAgentWriteStream(controller, w, payload) == nil
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if !write(first) {
		return
	}
	if stop := personalAgentStopEvent(snapshot.Status); stop != "" {
		payload, err := personalAgentStreamEvent(stop, snapshot)
		if err == nil {
			write(payload)
		}
		return
	}
	lastRevision := snapshot.Revision
	heartbeat := time.NewTicker(personalAgentHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case task, ok := <-updates:
			if !ok {
				return
			}
			if task.Revision <= lastRevision {
				continue
			}
			name := personalAgentStopEvent(task.Status)
			if name == "" {
				name = "task.updated"
			}
			payload, err := personalAgentStreamEvent(name, task)
			if err != nil || !write(payload) {
				return
			}
			lastRevision = task.Revision
			if personalAgentStopEvent(task.Status) != "" {
				return
			}
		case <-heartbeat.C:
			if !write([]byte(": heartbeat\n\n")) {
				return
			}
		}
	}
}

func personalAgentStopEvent(status string) string {
	switch status {
	case "awaiting_confirmation":
		return "task.waiting_confirmation"
	case "succeeded":
		return "task.succeeded"
	case "failed":
		return "task.failed"
	default:
		return ""
	}
}

func personalAgentStreamEvent(name string, task domain.PersonalAgentTask) ([]byte, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: %s\nid: %d\ndata: %s\n\n", name, task.Revision, data)), nil
}

func personalAgentWriteStream(controller *http.ResponseController, w http.ResponseWriter, payload []byte) error {
	if err := controller.SetWriteDeadline(time.Now().Add(personalAgentStreamWriteLimit)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	n, err := w.Write(payload)
	if err != nil {
		return err
	}
	if n != len(payload) {
		return io.ErrShortWrite
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	// The deadline bounds only this write and flush. Leaving it active while
	// waiting for the next update could expire an otherwise healthy HTTP/2
	// stream before the heartbeat arrives. Failed writes keep their deadline
	// so the HTTP server's final response flush remains bounded.
	if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
