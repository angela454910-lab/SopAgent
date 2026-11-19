package personalagentapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/application/personalagent"
	"agentflow/internal/domain"
)

type personalAgentStreamSpy struct {
	personalAgentAPISpy
	snapshot   domain.PersonalAgentTask
	updates    chan domain.PersonalAgentTask
	watchError error
	watchCalls atomic.Int32
	canceled   chan struct{}
	cancelOnce sync.Once
	watchID    string
	watchScope personalagent.Scope
}

func (s *personalAgentStreamSpy) Watch(_ context.Context, scope personalagent.Scope, id string) (domain.PersonalAgentTask, <-chan domain.PersonalAgentTask, func(), error) {
	s.watchCalls.Add(1)
	s.watchID, s.watchScope = id, scope
	return s.snapshot, s.updates, func() {
		s.cancelOnce.Do(func() {
			if s.canceled != nil {
				close(s.canceled)
			}
		})
	}, s.watchError
}

type personalAgentSSEFrame struct {
	name string
	id   string
	task domain.PersonalAgentTask
}

func personalAgentParseSSE(t *testing.T, text string) []personalAgentSSEFrame {
	t.Helper()
	frames := []personalAgentSSEFrame{}
	for _, block := range strings.Split(strings.TrimSpace(text), "\n\n") {
		if block == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var frame personalAgentSSEFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame.task); err != nil {
					t.Fatalf("invalid SSE task data: %v", err)
				}
			}
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestPersonalAgentSSEStreamsLatestSnapshotsUntilEachStopState(t *testing.T) {
	for _, tc := range []struct{ status, event string }{
		{"awaiting_confirmation", "task.waiting_confirmation"},
		{"succeeded", "task.succeeded"},
		{"failed", "task.failed"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			spy := &personalAgentStreamSpy{
				snapshot: domain.PersonalAgentTask{ID: "task-1", Revision: 2, Status: "planning"},
				updates:  make(chan domain.PersonalAgentTask, 4), canceled: make(chan struct{}),
			}
			// A stale or duplicate notification must not regress client state.
			spy.updates <- domain.PersonalAgentTask{ID: "task-1", Revision: 2, Status: "planning"}
			spy.updates <- domain.PersonalAgentTask{ID: "task-1", Revision: 1, Status: "planning"}
			spy.updates <- domain.PersonalAgentTask{ID: "task-1", Revision: 3, Status: "analyzing"}
			spy.updates <- domain.PersonalAgentTask{ID: "task-1", Revision: 4, Status: tc.status}
			w := httptest.NewRecorder()
			personalAgentTestRouter(t, spy).ServeHTTP(w, personalAgentTestRequest(http.MethodGet, "/v1/personal-agent/tasks/task-1/events", ""))
			if w.Code != http.StatusOK || !w.Flushed {
				t.Fatalf("status=%d flushed=%v body=%s", w.Code, w.Flushed, w.Body.String())
			}
			for key, want := range map[string]string{
				"Content-Type": "text/event-stream; charset=utf-8", "Cache-Control": "no-cache, no-transform",
				"X-Accel-Buffering": "no", "X-Content-Type-Options": "nosniff",
			} {
				if got := w.Header().Get(key); got != want {
					t.Fatalf("%s=%q, want %q", key, got, want)
				}
			}
			frames := personalAgentParseSSE(t, w.Body.String())
			if len(frames) != 3 || frames[0].name != "task.snapshot" || frames[0].id != "2" || frames[1].name != "task.updated" || frames[1].id != "3" || frames[2].name != tc.event || frames[2].id != "4" {
				t.Fatalf("unexpected frames: %+v", frames)
			}
			for _, frame := range frames {
				if frame.task.ID != "task-1" {
					t.Fatal("SSE data must be the direct complete task, not a task wrapper")
				}
			}
			select {
			case <-spy.canceled:
			default:
				t.Fatal("stop state must unsubscribe and close the response")
			}
			if spy.watchScope.ActorID != "owner" || spy.watchScope.ProjectID != "project-a" || spy.watchID != "task-1" {
				t.Fatalf("watch received wrong identity or resource: %+v, %s", spy.watchScope, spy.watchID)
			}
		})
	}
}

func TestPersonalAgentSSEStoppedSnapshotStillSendsStopAndDoesNotReplayCursor(t *testing.T) {
	for _, tc := range []struct{ status, event string }{
		{"awaiting_confirmation", "task.waiting_confirmation"}, {"succeeded", "task.succeeded"}, {"failed", "task.failed"},
	} {
		spy := &personalAgentStreamSpy{snapshot: domain.PersonalAgentTask{ID: "task-1", Revision: 9, Status: tc.status}}
		r := personalAgentTestRequest(http.MethodGet, "/v1/personal-agent/tasks/task-1/events", "")
		r.Header.Set("Last-Event-ID", "999")
		w := httptest.NewRecorder()
		personalAgentTestRouter(t, spy).ServeHTTP(w, r)
		frames := personalAgentParseSSE(t, w.Body.String())
		if len(frames) != 2 || frames[0].name != "task.snapshot" || frames[1].name != tc.event || frames[0].id != "9" || frames[1].id != "9" {
			t.Fatalf("must send latest snapshot and stop event regardless of old cursor: %+v", frames)
		}
	}
}

func TestPersonalAgentSSEPostNegotiationAndJSONCompatibility(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/tasks", `{"request":"analyze"}`},
		{"/tasks/task-1/feedback", `{"revision":3,"message":"correct this"}`},
		{"/tasks/task-1/confirm", `{"revision":3,"planHash":"hash"}`},
	} {
		for _, accept := range []string{"text/event-stream", "application/json", "text/event-stream;q=0"} {
			t.Run(tc.path+" "+accept, func(t *testing.T) {
				spy := &personalAgentStreamSpy{
					personalAgentAPISpy: personalAgentAPISpy{task: domain.PersonalAgentTask{ID: "task-1", Revision: 4, Status: "planning"}},
					snapshot:            domain.PersonalAgentTask{ID: "task-1", Revision: 5, Status: "awaiting_confirmation"},
				}
				r := personalAgentTestRequest(http.MethodPost, "/v1/personal-agent"+tc.path, tc.body)
				r.Header.Set("Accept", accept)
				w := httptest.NewRecorder()
				personalAgentTestRouter(t, spy).ServeHTTP(w, r)
				if spy.calls != 1 {
					t.Fatalf("expected exactly one mutation, got %d", spy.calls)
				}
				if accept == "text/event-stream" {
					frames := personalAgentParseSSE(t, w.Body.String())
					if w.Code != http.StatusOK || len(frames) != 2 || frames[0].task.Revision != 5 || spy.watchCalls.Load() != 1 || spy.watchID != "task-1" {
						t.Fatalf("POST must stream the latest task snapshot: status=%d frames=%+v watches=%d", w.Code, frames, spy.watchCalls.Load())
					}
				} else {
					var task domain.PersonalAgentTask
					if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
						t.Fatal(err)
					}
					if w.Code != http.StatusAccepted || task.ID != "task-1" || task.Revision != 4 || spy.watchCalls.Load() != 0 {
						t.Fatalf("JSON behavior changed: status=%d task=%+v watches=%d", w.Code, task, spy.watchCalls.Load())
					}
				}
			})
		}
	}
}

func TestPersonalAgentSSEErrorsRemainJSONBeforeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
		mutate func(*http.Request)
		err    error
		status int
	}{
		{"invalid auth", http.MethodGet, "/tasks/task-1/events", "", func(r *http.Request) { r.Header.Del("Authorization") }, nil, http.StatusUnauthorized},
		{"missing identity", http.MethodGet, "/tasks/task-1/events", "", func(r *http.Request) { r.Header.Del("X-Actor-ID") }, nil, http.StatusBadRequest},
		{"not found", http.MethodGet, "/tasks/missing/events", "", nil, personalagent.ErrNotFound, http.StatusNotFound},
		{"stale confirm", http.MethodPost, "/tasks/task-1/confirm", `{"revision":2,"planHash":"old"}`, nil, personalagent.ErrConflict, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &personalAgentStreamSpy{personalAgentAPISpy: personalAgentAPISpy{err: tc.err}, watchError: tc.err}
			r := personalAgentTestRequest(tc.method, "/v1/personal-agent"+tc.path, tc.body)
			r.Header.Set("Accept", "text/event-stream")
			if tc.mutate != nil {
				tc.mutate(r)
			}
			w := httptest.NewRecorder()
			personalAgentTestRouter(t, spy).ServeHTTP(w, r)
			if w.Code != tc.status || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || strings.Contains(w.Body.String(), "event:") {
				t.Fatalf("status=%d type=%s body=%s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			if tc.name != "not found" && spy.watchCalls.Load() != 0 {
				t.Fatal("failed authorization or mutation must not subscribe")
			}
		})
	}
}

func TestPersonalAgentSSEDisconnectOnlyCancelsSubscription(t *testing.T) {
	spy := &personalAgentStreamSpy{
		snapshot: domain.PersonalAgentTask{ID: "task-1", Revision: 1, Status: "analyzing"},
		updates:  make(chan domain.PersonalAgentTask), canceled: make(chan struct{}),
	}
	server := httptest.NewServer(personalAgentTestRouter(t, spy))
	defer server.Close()
	r, err := http.NewRequest(http.MethodGet, server.URL+"/v1/personal-agent/tasks/task-1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header = personalAgentTestRequest(http.MethodGet, "/", "").Header
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		if line == "\n" {
			break
		}
	}
	_ = response.Body.Close()
	select {
	case <-spy.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected client left a subscription behind")
	}
	if spy.calls != 0 || spy.watchCalls.Load() != 1 {
		t.Fatal("stream disconnect must only unsubscribe, never mutate or cancel task work")
	}
}

type personalAgentNoFlushWriter struct{ recorder *httptest.ResponseRecorder }

func (w personalAgentNoFlushWriter) Header() http.Header            { return w.recorder.Header() }
func (w personalAgentNoFlushWriter) WriteHeader(code int)           { w.recorder.WriteHeader(code) }
func (w personalAgentNoFlushWriter) Write(data []byte) (int, error) { return w.recorder.Write(data) }

type personalAgentFlushErrorWriter struct{ *httptest.ResponseRecorder }

func (w personalAgentFlushErrorWriter) FlushError() error { return io.ErrClosedPipe }

type personalAgentWriteErrorWriter struct{ *httptest.ResponseRecorder }

func (w personalAgentWriteErrorWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPersonalAgentSSEHandlesUnavailableFlusherAndWriteFailures(t *testing.T) {
	spy := &personalAgentStreamSpy{}
	r := personalAgentTestRequest(http.MethodPost, "/v1/personal-agent/tasks", `{"request":"analyze"}`)
	r.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	personalAgentTestRouter(t, spy).ServeHTTP(personalAgentNoFlushWriter{w}, r)
	if w.Code != http.StatusInternalServerError || spy.calls != 0 || spy.watchCalls.Load() != 0 {
		t.Fatalf("unsupported flusher must fail before creating work: status=%d calls=%d watches=%d", w.Code, spy.calls, spy.watchCalls.Load())
	}
	for _, writer := range []func(*httptest.ResponseRecorder) http.ResponseWriter{
		func(w *httptest.ResponseRecorder) http.ResponseWriter { return personalAgentFlushErrorWriter{w} },
		func(w *httptest.ResponseRecorder) http.ResponseWriter { return personalAgentWriteErrorWriter{w} },
	} {
		spy = &personalAgentStreamSpy{
			snapshot: domain.PersonalAgentTask{ID: "task-1", Revision: 1, Status: "analyzing"},
			canceled: make(chan struct{}),
		}
		w = httptest.NewRecorder()
		personalAgentTestRouter(t, spy).ServeHTTP(writer(w), personalAgentTestRequest(http.MethodGet, "/v1/personal-agent/tasks/task-1/events", ""))
		select {
		case <-spy.canceled:
		default:
			t.Fatal("write or flush failure must stop streaming and unsubscribe")
		}
	}
}

func TestPersonalAgentSSEAcceptQuality(t *testing.T) {
	for _, accept := range []string{"text/event-stream;q=0", "text/event-stream;q=NaN", "text/event-stream;q=invalid", "application/json"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept", accept)
		if personalAgentWantsStream(r) {
			t.Fatalf("invalid or zero-quality SSE preference accepted: %s", accept)
		}
	}
	for _, accept := range []string{"text/event-stream", "application/json, text/event-stream;q=0.8"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept", accept)
		if !personalAgentWantsStream(r) {
			t.Fatalf("SSE preference was not accepted: %s", accept)
		}
	}
}

type personalAgentDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	err       error
	flushErr  error
}

func (w *personalAgentDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return w.err
}

func (w *personalAgentDeadlineWriter) FlushError() error {
	if w.flushErr != nil {
		return w.flushErr
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestPersonalAgentSSEBoundsEachWriteAndPropagatesDeadlineErrors(t *testing.T) {
	w := &personalAgentDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	if err := personalAgentWriteStream(http.NewResponseController(w), w, []byte(": heartbeat\n\n")); err != nil {
		t.Fatal(err)
	}
	if len(w.deadlines) != 2 || w.deadlines[0].Before(start.Add(14*time.Second)) || w.deadlines[0].After(time.Now().Add(16*time.Second)) || !w.deadlines[1].IsZero() || !w.Flushed {
		t.Fatalf("each flushed write must set then immediately clear its deadline: %+v, flushed=%v", w.deadlines, w.Flushed)
	}
	failure := errors.New("transport cannot set a write deadline")
	w = &personalAgentDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), err: failure}
	if err := personalAgentWriteStream(http.NewResponseController(w), w, []byte("must not write")); !errors.Is(err, failure) || w.Body.Len() != 0 {
		t.Fatalf("deadline failure must stop before writing: err=%v bytes=%d", err, w.Body.Len())
	}
	w = &personalAgentDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), flushErr: io.ErrClosedPipe}
	if err := personalAgentWriteStream(http.NewResponseController(w), w, []byte(": heartbeat\n\n")); !errors.Is(err, io.ErrClosedPipe) || len(w.deadlines) != 1 || w.deadlines[0].IsZero() {
		t.Fatalf("flush failure must preserve the active deadline: err=%v deadlines=%+v", err, w.deadlines)
	}
}

func TestPersonalAgentSSEClearsDeadlineAfterEachHealthyFlush(t *testing.T) {
	for _, fail := range []bool{false, true} {
		spy := &personalAgentStreamSpy{snapshot: domain.PersonalAgentTask{ID: "task-1", Revision: 1, Status: "succeeded"}}
		w := &personalAgentDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		if fail {
			w.err = errors.New("connection deadline failed")
		}
		personalAgentTestRouter(t, spy).ServeHTTP(w, personalAgentTestRequest(http.MethodGet, "/v1/personal-agent/tasks/task-1/events", ""))
		last := w.deadlines[len(w.deadlines)-1]
		if !fail && (len(w.deadlines) != 4 || !w.deadlines[1].IsZero() || w.deadlines[2].IsZero() || !last.IsZero()) {
			t.Fatalf("snapshot and terminal flush must each set and clear their own deadline: %+v", w.deadlines)
		}
		if fail && last.IsZero() {
			t.Fatal("failed writes must keep their deadline to bound the server's final flush")
		}
	}
}
