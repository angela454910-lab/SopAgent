package personalagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

type watchReadBarrier struct {
	entered chan struct{}
	release chan struct{}
}

type watchCountingStore struct {
	Store
	reads    atomic.Int64
	mu       sync.Mutex
	nextRead *watchReadBarrier
}

func (store *watchCountingStore) PersonalAgentTask(project, actor, id string) (domain.PersonalAgentTask, error) {
	store.reads.Add(1)
	task, err := store.Store.PersonalAgentTask(project, actor, id)
	store.mu.Lock()
	barrier := store.nextRead
	store.nextRead = nil
	store.mu.Unlock()
	if barrier != nil {
		close(barrier.entered)
		<-barrier.release
	}
	return task, err
}

func newWatchFixture(t *testing.T) (*Service, *watchCountingStore, Scope, domain.PersonalAgentTask) {
	t.Helper()
	db, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	store := &watchCountingStore{Store: db}
	service := NewService(store, nil)
	t.Cleanup(service.Close)
	scope := Scope{ProjectID: "stream-project", ActorID: "stream-actor"}
	now := time.Now().UTC()
	task := domain.PersonalAgentTask{ID: newID("task"), ProjectID: scope.ProjectID, ActorID: scope.ActorID, Request: "Inspect the supplied input", Context: map[string]any{"nested": map[string]any{"value": "original"}}, Status: "planning", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreatePersonalAgentTask(task); err != nil {
		t.Fatal(err)
	}
	return service, store, scope, task
}

func nextWatchSnapshot(t *testing.T, updates <-chan domain.PersonalAgentTask) domain.PersonalAgentTask {
	t.Helper()
	select {
	case task, ok := <-updates:
		if !ok {
			t.Fatal("subscription closed before update")
		}
		return task
	case <-time.After(5 * time.Second):
		t.Fatal("subscription did not receive update")
		return domain.PersonalAgentTask{}
	}
}

func requireWatchClosed(t *testing.T, updates <-chan domain.PersonalAgentTask) {
	t.Helper()
	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("subscription should be closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription did not close")
	}
}

func TestPersonalAgentWatchAtomicSnapshotAndRegistration(t *testing.T) {
	s, store, scope, task := newWatchFixture(t)
	barrier := &watchReadBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	store.mu.Lock()
	store.nextRead = barrier
	store.mu.Unlock()
	type result struct {
		initial domain.PersonalAgentTask
		updates <-chan domain.PersonalAgentTask
		cancel  func()
		err     error
	}
	registered := make(chan result, 1)
	go func() {
		initial, updates, cancel, err := s.Watch(context.Background(), scope, task.ID)
		registered <- result{initial, updates, cancel, err}
	}()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot read did not start")
	}
	mutationStarted := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		close(mutationStarted)
		mutationDone <- s.change(scope, task.ID, func(task *domain.PersonalAgentTask) error { task.Status = "analyzing"; return nil })
	}()
	<-mutationStarted
	close(barrier.release)
	var subscribed result
	select {
	case subscribed = <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot registration did not complete")
	}
	if subscribed.err != nil {
		t.Fatal(subscribed.err)
	}
	defer subscribed.cancel()
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if subscribed.initial.Revision != 1 {
		t.Fatalf("snapshot should precede mutation: %d", subscribed.initial.Revision)
	}
	update := nextWatchSnapshot(t, subscribed.updates)
	if update.Revision != 2 || update.Status != "analyzing" {
		t.Fatalf("update lost between read and subscription: %#v", update)
	}
}

func TestPersonalAgentWatchScopeCancellationCloseAndNoPolling(t *testing.T) {
	s, store, scope, task := newWatchFixture(t)
	for _, foreign := range []Scope{{ProjectID: "other-project", ActorID: scope.ActorID}, {ProjectID: scope.ProjectID, ActorID: "other-actor"}} {
		if _, _, cancel, err := s.Watch(context.Background(), foreign, task.ID); !errors.Is(err, ErrNotFound) || cancel != nil {
			t.Fatalf("foreign task subscribed: %v", err)
		}
	}
	if _, _, _, err := s.Watch(context.Background(), Scope{}, task.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid scope: %v", err)
	}
	cancelled, cancelBeforeWatch := context.WithCancel(context.Background())
	cancelBeforeWatch()
	if _, _, _, err := s.Watch(cancelled, scope, task.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context subscribed: %v", err)
	}
	ctx, cancelContext := context.WithCancel(context.Background())
	initial, updates, cancel, err := s.Watch(ctx, scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.ID != task.ID || initial.Revision != 1 {
		t.Fatal("initial snapshot missing")
	}
	reads := store.reads.Load()
	select {
	case <-updates:
		t.Fatal("idle subscription emitted an update")
	case <-time.After(30 * time.Millisecond):
	}
	if store.reads.Load() != reads {
		t.Fatal("idle subscription polled storage")
	}
	cancelContext()
	requireWatchClosed(t, updates)
	cancel()
	cancel()
	s.mu.Lock()
	remaining := len(s.watchers)
	s.mu.Unlock()
	if remaining != 0 {
		t.Fatal("cancelled subscription leaked")
	}
	_, updates, cancel, err = s.Watch(context.Background(), scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	requireWatchClosed(t, updates)
	cancel()
	if _, _, _, err := s.Watch(context.Background(), scope, task.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("closed service accepted subscription: %v", err)
	}
}

func TestPersonalAgentWatchSlowClientRetainsTerminalAndIsolatesSnapshots(t *testing.T) {
	s, _, scope, task := newWatchFixture(t)
	_, first, cancelFirst, err := s.Watch(context.Background(), scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelFirst()
	_, second, cancelSecond, err := s.Watch(context.Background(), scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelSecond()
	if err := s.change(scope, task.ID, func(task *domain.PersonalAgentTask) error {
		task.Context["nested"].(map[string]any)["value"] = "updated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	firstUpdate := nextWatchSnapshot(t, first)
	firstUpdate.Context["nested"].(map[string]any)["value"] = "recipient mutation"
	secondUpdate := nextWatchSnapshot(t, second)
	if secondUpdate.Context["nested"].(map[string]any)["value"] != "updated" {
		t.Fatal("subscribers share mutable snapshots")
	}
	for n := 0; n < 30; n++ {
		if err := s.change(scope, task.ID, func(task *domain.PersonalAgentTask) error { task.Status = "analyzing"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.change(scope, task.ID, func(task *domain.PersonalAgentTask) error { task.Status = "succeeded"; return nil }); err != nil {
		t.Fatal(err)
	}
	// Closing a subscription preserves its buffered latest snapshot.
	cancelFirst()
	terminal := nextWatchSnapshot(t, first)
	if terminal.Status != "succeeded" || terminal.Revision != 33 {
		t.Fatalf("slow client lost terminal snapshot: %#v", terminal)
	}
	requireWatchClosed(t, first)
	if update := nextWatchSnapshot(t, second); update.Revision != terminal.Revision || update.Status != "succeeded" {
		t.Fatal("subscribers did not receive latest terminal state")
	}
}

type watchRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip watchRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return roundTrip(r)
}

func TestPersonalAgentWatchDisconnectDoesNotCancelTaskAndPublishesMutations(t *testing.T) {
	db, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plannerStarted := make(chan struct{})
	releasePlanner := make(chan struct{})
	var started sync.Once
	plan := domain.PersonalAgentPlan{Name: "Generated report", Description: "Analyze current task inputs", Agents: []domain.PersonalAgentSpec{{ID: "dynamic-reader", Name: "Dynamic reader", Purpose: "Inspect the request", Instructions: "Summarize current request", Phase: "analysis", OutputContract: "summary and questions"}}}
	gateway, _ := NewHTTPGateway("http://gateway.invalid", "test-token")
	gateway.client.Transport = watchRoundTripper(func(r *http.Request) (*http.Response, error) {
		var result any
		switch r.URL.Path {
		case "/capabilities":
			result = map[string]any{"tools": []any{}}
		case "/model":
			var input struct {
				Purpose string `json:"purpose"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			if input.Purpose == "planner" {
				started.Do(func() { close(plannerStarted) })
				select {
				case <-releasePlanner:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				result = map[string]any{"content": asJSON(plan)}
			} else {
				result = map[string]any{"content": asJSON(map[string]any{"action": "complete", "output": map[string]any{"summary": "current input analyzed", "questions": []any{}}})}
			}
		default:
			return nil, errors.New("unexpected gateway request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(asJSON(result))), Request: r}, nil
	})
	s := NewService(db, gateway)
	defer s.Close()
	scope := Scope{ProjectID: "p", ActorID: "a"}
	task, err := s.Create(context.Background(), scope, CreateRequest{Request: "Analyze the current input"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-plannerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("planner did not start")
	}
	connection, cancelConnection := context.WithCancel(context.Background())
	initial, updates, cancelWatch, err := s.Watch(connection, scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Status != "planning" {
		t.Fatal("expected blocked planner snapshot")
	}
	cancelConnection()
	requireWatchClosed(t, updates)
	cancelWatch()
	initial, updates, cancelWatch, err = s.Watch(context.Background(), scope, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelWatch()
	close(releasePlanner)
	task = awaitWatchStatus(t, initial, updates, "awaiting_confirmation")
	feedback, err := s.Feedback(context.Background(), scope, task.ID, FeedbackRequest{Revision: task.Revision, Message: "Use the current request only"})
	if err != nil {
		t.Fatal(err)
	}
	task = awaitWatchStatus(t, feedback, updates, "awaiting_confirmation")
	if len(task.Feedback) != 1 {
		t.Fatal("feedback update missing")
	}
	confirmed, err := s.Confirm(context.Background(), scope, task.ID, ConfirmRequest{Revision: task.Revision, PlanHash: task.PlanHash})
	if err != nil {
		t.Fatal(err)
	}
	task = awaitWatchStatus(t, confirmed, updates, "succeeded")
	if task.ApprovedAt == nil || task.SOPID == "" {
		t.Fatal("task did not finish after first client disconnected")
	}
}

func awaitWatchStatus(t *testing.T, initial domain.PersonalAgentTask, updates <-chan domain.PersonalAgentTask, status string) domain.PersonalAgentTask {
	t.Helper()
	latest := initial
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for latest.Status != status {
		if latest.Status == "failed" {
			t.Fatalf("task failed while streaming: %s", latest.Error)
		}
		select {
		case update, ok := <-updates:
			if !ok {
				t.Fatal("stream closed before desired status")
			}
			if update.Revision >= latest.Revision {
				latest = update
			}
		case <-deadline.C:
			t.Fatalf("stream never reached %s; last=%s", status, latest.Status)
		}
	}
	return latest
}
