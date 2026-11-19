package personalagentapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/application/personalagent"
	"agentflow/internal/domain"
)

type personalAgentAPISpy struct {
	calls   int
	scope   personalagent.Scope
	confirm personalagent.ConfirmRequest
	err     error
}

func (s *personalAgentAPISpy) Create(_ context.Context, scope personalagent.Scope, _ personalagent.CreateRequest) (domain.PersonalAgentTask, error) {
	s.calls++
	s.scope = scope
	return domain.PersonalAgentTask{}, s.err
}

func (s *personalAgentAPISpy) Get(_ context.Context, scope personalagent.Scope, _ string) (domain.PersonalAgentTask, error) {
	s.calls++
	s.scope = scope
	// The application performs the ownership lookup. This double checks that
	// the transport forwards the requesting actor and preserves a 404 result.
	if scope.ActorID != "owner" {
		return domain.PersonalAgentTask{}, personalagent.ErrNotFound
	}
	return domain.PersonalAgentTask{}, s.err
}

func (s *personalAgentAPISpy) List(_ context.Context, scope personalagent.Scope) ([]domain.PersonalAgentTask, error) {
	s.calls++
	s.scope = scope
	return nil, s.err
}

func (s *personalAgentAPISpy) Feedback(_ context.Context, scope personalagent.Scope, _ string, _ personalagent.FeedbackRequest) (domain.PersonalAgentTask, error) {
	s.calls++
	s.scope = scope
	return domain.PersonalAgentTask{}, s.err
}

func (s *personalAgentAPISpy) Confirm(_ context.Context, scope personalagent.Scope, _ string, request personalagent.ConfirmRequest) (domain.PersonalAgentTask, error) {
	s.calls++
	s.scope = scope
	s.confirm = request
	return domain.PersonalAgentTask{}, s.err
}

func (s *personalAgentAPISpy) SOPs(_ context.Context, scope personalagent.Scope) ([]domain.PersonalAgentSOP, error) {
	s.calls++
	s.scope = scope
	return nil, s.err
}

func (s *personalAgentAPISpy) Capabilities(_ context.Context, scope personalagent.Scope) ([]domain.PersonalAgentTool, error) {
	s.calls++
	s.scope = scope
	return nil, s.err
}

func personalAgentTestRouter(t *testing.T, service PersonalAgentService) http.Handler {
	t.Helper()
	router, err := NewPersonalAgentRouter(service, "test-service-token")
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func personalAgentTestRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer test-service-token")
	r.Header.Set("X-Project-ID", "project-a")
	r.Header.Set("X-Actor-ID", "owner")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func TestPersonalAgentAPIFailsClosedWithoutTrustedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"missing token", func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }, http.StatusUnauthorized},
		{"missing actor", func(r *http.Request) { r.Header.Del("X-Actor-ID") }, http.StatusBadRequest},
		{"missing project", func(r *http.Request) { r.Header.Del("X-Project-ID") }, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &personalAgentAPISpy{}
			r := personalAgentTestRequest(http.MethodPost, "/v1/personal-agent/tasks", `{"request":"analyze"}`)
			tc.mutate(r)
			w := httptest.NewRecorder()
			personalAgentTestRouter(t, spy).ServeHTTP(w, r)
			if w.Code != tc.status || spy.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, spy.calls, w.Body.String())
			}
		})
	}
	if _, err := NewPersonalAgentRouter(&personalAgentAPISpy{}, " "); err == nil {
		t.Fatal("empty token must not create an unauthenticated service")
	}
}

func TestPersonalAgentAPIBodyCannotOverrideIdentity(t *testing.T) {
	spy := &personalAgentAPISpy{}
	router := personalAgentTestRouter(t, spy)
	for _, body := range []string{
		`{"request":"analyze","actorId":"other"}`,
		`{"request":"analyze","projectId":"other"}`,
		`{"request":"analyze"} {"request":"different"}`,
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, personalAgentTestRequest(http.MethodPost, "/v1/personal-agent/tasks", body))
		if w.Code != http.StatusBadRequest || spy.calls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", w.Code, spy.calls, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, personalAgentTestRequest(http.MethodPost, "/v1/personal-agent/tasks", `{"request":"analyze","context":{"fileId":"attachment-1"}}`))
	if w.Code != http.StatusAccepted || spy.scope.ProjectID != "project-a" || spy.scope.ActorID != "owner" {
		t.Fatalf("status=%d scope=%+v body=%s", w.Code, spy.scope, w.Body.String())
	}
}

func TestPersonalAgentAPIConfirmPreservesExpectedRevision(t *testing.T) {
	spy := &personalAgentAPISpy{err: personalagent.ErrConflict}
	w := httptest.NewRecorder()
	personalAgentTestRouter(t, spy).ServeHTTP(w, personalAgentTestRequest(http.MethodPost, "/v1/personal-agent/tasks/task-1/confirm", `{"revision":7,"planHash":"approved-hash"}`))
	if w.Code != http.StatusConflict || spy.confirm.Revision != 7 || spy.confirm.PlanHash != "approved-hash" {
		t.Fatalf("status=%d confirmation=%+v body=%s", w.Code, spy.confirm, w.Body.String())
	}
}

func TestPersonalAgentAPIForwardsActorScopeAndMapsNotFound(t *testing.T) {
	spy := &personalAgentAPISpy{}
	r := personalAgentTestRequest(http.MethodGet, "/v1/personal-agent/tasks/task-1", "")
	r.Header.Set("X-Actor-ID", "other")
	w := httptest.NewRecorder()
	personalAgentTestRouter(t, spy).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || spy.scope.ActorID != "other" {
		t.Fatalf("status=%d scope=%+v body=%s", w.Code, spy.scope, w.Body.String())
	}
}

func TestPersonalAgentAPIRejectsOversizedBodyAndExposesOnlyCoarseHealth(t *testing.T) {
	spy := &personalAgentAPISpy{}
	router := personalAgentTestRouter(t, spy)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, personalAgentTestRequest(http.MethodPost, "/v1/personal-agent/tasks", `{"request":"`+strings.Repeat("x", personalAgentMaxBody)+`"}`))
	if w.Code != http.StatusRequestEntityTooLarge || spy.calls != 0 {
		t.Fatalf("status=%d calls=%d", w.Code, spy.calls)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != http.StatusOK || spy.calls != 0 || strings.Contains(w.Body.String(), "gateway") {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, spy.calls, w.Body.String())
	}
}
