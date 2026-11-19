// Package personalagentapi exposes the embedded Agent service without linking
// Studio's web assets or its platform-specific HTTP assembly.
package personalagentapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode"

	"agentflow/internal/application/personalagent"
	"agentflow/internal/domain"
)

// PersonalAgentService is the application boundary used by the headless API.
// Identity is supplied by the authenticated host backend, never a JSON body.
type PersonalAgentService interface {
	Create(context.Context, personalagent.Scope, personalagent.CreateRequest) (domain.PersonalAgentTask, error)
	Get(context.Context, personalagent.Scope, string) (domain.PersonalAgentTask, error)
	List(context.Context, personalagent.Scope) ([]domain.PersonalAgentTask, error)
	Feedback(context.Context, personalagent.Scope, string, personalagent.FeedbackRequest) (domain.PersonalAgentTask, error)
	Confirm(context.Context, personalagent.Scope, string, personalagent.ConfirmRequest) (domain.PersonalAgentTask, error)
	SOPs(context.Context, personalagent.Scope) ([]domain.PersonalAgentSOP, error)
	Capabilities(context.Context, personalagent.Scope) ([]domain.PersonalAgentTool, error)
}

// personalAgentHandlers owns only the embedded Personal Agent API. The Studio
// router, cookies, CLI executors and repository integrations are not mounted.
type personalAgentHandlers struct {
	service PersonalAgentService
	token   [sha256.Size]byte
}

const personalAgentMaxBody = 2 << 20

// NewPersonalAgentRouter assembles the standalone transport around the shared
// application service. Callers must provide a nonempty server-to-server token.
func NewPersonalAgentRouter(service PersonalAgentService, token string) (http.Handler, error) {
	if service == nil {
		return nil, errors.New("personal agent service is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("SOP_AGENT_SERVICE_TOKEN is required")
	}
	h := &personalAgentHandlers{service: service, token: sha256.Sum256([]byte(token))}
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/personal-agent/capabilities", h.capabilities)
	api.HandleFunc("GET /v1/personal-agent/tasks", h.list)
	api.HandleFunc("POST /v1/personal-agent/tasks", h.create)
	api.HandleFunc("GET /v1/personal-agent/tasks/{id}", h.get)
	api.HandleFunc("POST /v1/personal-agent/tasks/{id}/confirm", h.confirm)
	api.HandleFunc("POST /v1/personal-agent/tasks/{id}/feedback", h.feedback)
	api.HandleFunc("GET /v1/personal-agent/sops", h.sops)
	api.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		personalAgentJSON(w, http.StatusNotFound, map[string]string{"error": "endpoint not found"})
	})
	root := http.NewServeMux()
	root.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		personalAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "personal-agent"})
	})
	root.Handle("/", h.authorize(api))
	return root, nil
}

type personalAgentScopeKey struct{}

func (h *personalAgentHandlers) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		candidate := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
		if !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare(candidate[:], h.token[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			personalAgentJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid service credentials"})
			return
		}
		scope := personalagent.Scope{
			ProjectID: strings.TrimSpace(r.Header.Get("X-Project-ID")),
			ActorID:   strings.TrimSpace(r.Header.Get("X-Actor-ID")),
		}
		if !personalAgentIdentityValid(scope.ProjectID) || !personalAgentIdentityValid(scope.ActorID) {
			personalAgentJSON(w, http.StatusBadRequest, map[string]string{"error": "X-Project-ID and X-Actor-ID are required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), personalAgentScopeKey{}, scope)))
	})
}

func personalAgentIdentityValid(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsFunc(value, unicode.IsControl)
}

func personalAgentScope(r *http.Request) personalagent.Scope {
	return r.Context().Value(personalAgentScopeKey{}).(personalagent.Scope)
}

func (h *personalAgentHandlers) capabilities(w http.ResponseWriter, r *http.Request) {
	tools, err := h.service.Capabilities(r.Context(), personalAgentScope(r))
	if err != nil {
		personalAgentError(w, err)
		return
	}
	if tools == nil {
		tools = []domain.PersonalAgentTool{}
	}
	personalAgentJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func (h *personalAgentHandlers) list(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.service.List(r.Context(), personalAgentScope(r))
	if err != nil {
		personalAgentError(w, err)
		return
	}
	if tasks == nil {
		tasks = []domain.PersonalAgentTask{}
	}
	personalAgentJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (h *personalAgentHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body personalagent.CreateRequest
	if !personalAgentDecode(w, r, &body) {
		return
	}
	task, err := h.service.Create(r.Context(), personalAgentScope(r), body)
	if err != nil {
		personalAgentError(w, err)
		return
	}
	personalAgentJSON(w, http.StatusAccepted, task)
}

func (h *personalAgentHandlers) get(w http.ResponseWriter, r *http.Request) {
	task, err := h.service.Get(r.Context(), personalAgentScope(r), r.PathValue("id"))
	if err != nil {
		personalAgentError(w, err)
		return
	}
	personalAgentJSON(w, http.StatusOK, task)
}

func (h *personalAgentHandlers) confirm(w http.ResponseWriter, r *http.Request) {
	var body personalagent.ConfirmRequest
	if !personalAgentDecode(w, r, &body) {
		return
	}
	task, err := h.service.Confirm(r.Context(), personalAgentScope(r), r.PathValue("id"), body)
	if err != nil {
		personalAgentError(w, err)
		return
	}
	personalAgentJSON(w, http.StatusAccepted, task)
}

func (h *personalAgentHandlers) feedback(w http.ResponseWriter, r *http.Request) {
	var body personalagent.FeedbackRequest
	if !personalAgentDecode(w, r, &body) {
		return
	}
	task, err := h.service.Feedback(r.Context(), personalAgentScope(r), r.PathValue("id"), body)
	if err != nil {
		personalAgentError(w, err)
		return
	}
	personalAgentJSON(w, http.StatusAccepted, task)
}

func (h *personalAgentHandlers) sops(w http.ResponseWriter, r *http.Request) {
	sops, err := h.service.SOPs(r.Context(), personalAgentScope(r))
	if err != nil {
		personalAgentError(w, err)
		return
	}
	if sops == nil {
		sops = []domain.PersonalAgentSOP{}
	}
	personalAgentJSON(w, http.StatusOK, map[string]any{"sops": sops})
}

func personalAgentDecode(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		personalAgentJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, personalAgentMaxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(dst)
	if err == nil {
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			err = errors.New("one JSON object is required")
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		personalAgentJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds 2 MiB"})
	} else {
		personalAgentJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body or unsupported field"})
	}
	return false
}

func personalAgentError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "personal agent request failed"
	switch {
	case errors.Is(err, personalagent.ErrNotFound):
		status, message = http.StatusNotFound, "task or SOP not found"
	case errors.Is(err, personalagent.ErrConflict):
		status, message = http.StatusConflict, "task changed; refresh before confirming or editing"
	case errors.Is(err, personalagent.ErrInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, personalagent.ErrGateway):
		status, message = http.StatusBadGateway, "host model or tool gateway is unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "upstream request timed out"
	case errors.Is(err, context.Canceled):
		status, message = http.StatusRequestTimeout, "request canceled"
	}
	personalAgentJSON(w, status, map[string]string{"error": message})
}

func personalAgentJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
