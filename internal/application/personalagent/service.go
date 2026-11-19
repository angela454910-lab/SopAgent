// Package personalagent implements host-neutral dynamic role planning and SOP
// execution. Business records and side effects live behind a scoped gateway.
package personalagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

var (
	ErrInvalid  = errors.New("invalid personal agent request")
	ErrNotFound = errors.New("personal agent resource not found")
	ErrConflict = errors.New("personal agent state changed")
	ErrGateway  = errors.New("personal agent gateway failed")
)

type Scope struct {
	ProjectID string
	ActorID   string
}
type CreateRequest struct {
	Request string         `json:"request"`
	Context map[string]any `json:"context"`
	SOPID   string         `json:"sopId,omitempty"`
}
type FeedbackRequest struct {
	Revision int    `json:"revision"`
	Message  string `json:"message"`
}
type ConfirmRequest struct {
	Revision int    `json:"revision"`
	PlanHash string `json:"planHash"`
}

type Store interface {
	CreatePersonalAgentTask(domain.PersonalAgentTask) error
	PersonalAgentTask(string, string, string) (domain.PersonalAgentTask, error)
	PersonalAgentTasks(string, string) ([]domain.PersonalAgentTask, error)
	ActivePersonalAgentTasks() ([]domain.PersonalAgentTask, error)
	UpdatePersonalAgentTask(domain.PersonalAgentTask, int) error
	ConfirmPersonalAgentTask(domain.PersonalAgentTask, int, *domain.PersonalAgentSOP) error
	PersonalAgentSOP(string, string, string) (domain.PersonalAgentSOP, error)
	PersonalAgentSOPs(string, string) ([]domain.PersonalAgentSOP, error)
}

type Service struct {
	store    Store
	gateway  *HTTPGateway
	mu       sync.Mutex
	workers  map[string]string
	watchers map[taskWatchKey]map[*taskWatcher]struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
}

func NewService(store Store, gateway *HTTPGateway) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{store: store, gateway: gateway, workers: map[string]string{}, watchers: map[taskWatchKey]map[*taskWatcher]struct{}{}, ctx: ctx, cancel: cancel}
}
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.closeWatchersLocked()
	s.mu.Unlock()
	s.wg.Wait()
}

// RecoverInterrupted must run before accepting requests with one Personal
// Agent worker per database. It records interrupted work and never replays
// effects whose outcome may be unknown. This is runtime recovery, not a seed.
func (s *Service) RecoverInterrupted(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.workers) > 0 {
		return ErrConflict
	}
	tasks, err := s.store.ActivePersonalAgentTasks()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return err
		}
		expected := task.Revision
		task.Status = "failed"
		task.Error = "服务在执行期间中断；未自动重放任务。请核对工具证据和外部状态。"
		task.Revision++
		task.UpdatedAt = time.Now().UTC()
		for i := range task.Runs {
			run := &task.Runs[i]
			if run.Status == "running" {
				run.Status = "failed"
				run.Error = task.Error
				run.FinishedAt = &task.UpdatedAt
			}
			for j := range run.ToolCalls {
				call := &run.ToolCalls[j]
				if call.Status == "running" {
					call.Status = "failed"
					if call.Effect == "write" {
						call.Status = "unknown"
					}
					call.Error = task.Error
				}
			}
		}
		addEvent(&task, "task.interrupted", task.Error, "")
		if err := s.store.UpdatePersonalAgentTask(task, expected); err != nil {
			return storeError(err)
		}
		s.publishLocked(task)
	}
	return nil
}

func scopeValid(scope Scope) error {
	if strings.TrimSpace(scope.ProjectID) == "" || strings.TrimSpace(scope.ActorID) == "" || len(scope.ProjectID) > 128 || len(scope.ActorID) > 128 {
		return fmt.Errorf("%w: project and actor scope are required", ErrInvalid)
	}
	return nil
}
func storeError(err error) error {
	if repo.IsPersonalAgentNotFound(err) {
		return ErrNotFound
	}
	if errors.Is(err, repo.ErrRunDecisionStale) {
		return ErrConflict
	}
	return err
}
func (s *Service) Get(ctx context.Context, scope Scope, id string) (domain.PersonalAgentTask, error) {
	if err := scopeValid(scope); err != nil {
		return domain.PersonalAgentTask{}, err
	}
	t, err := s.store.PersonalAgentTask(scope.ProjectID, scope.ActorID, id)
	return t, storeError(err)
}
func (s *Service) List(ctx context.Context, scope Scope) ([]domain.PersonalAgentTask, error) {
	if err := scopeValid(scope); err != nil {
		return nil, err
	}
	return s.store.PersonalAgentTasks(scope.ProjectID, scope.ActorID)
}
func (s *Service) SOPs(ctx context.Context, scope Scope) ([]domain.PersonalAgentSOP, error) {
	if err := scopeValid(scope); err != nil {
		return nil, err
	}
	return s.store.PersonalAgentSOPs(scope.ProjectID, scope.ActorID)
}
func (s *Service) Capabilities(ctx context.Context, scope Scope) ([]domain.PersonalAgentTool, error) {
	if err := scopeValid(scope); err != nil {
		return nil, err
	}
	return s.gateway.capabilities(ctx, scope)
}

func (s *Service) Create(ctx context.Context, scope Scope, in CreateRequest) (domain.PersonalAgentTask, error) {
	var empty domain.PersonalAgentTask
	if err := scopeValid(scope); err != nil {
		return empty, err
	}
	in.Request = strings.TrimSpace(in.Request)
	data, err := json.Marshal(in.Context)
	if err != nil || len(data) > 1024*1024 || in.Request == "" || len(in.Request) > 20000 {
		return empty, fmt.Errorf("%w: request required (max 20,000 bytes), context max 1 MiB", ErrInvalid)
	}
	if in.SOPID != "" {
		if _, err := s.store.PersonalAgentSOP(scope.ProjectID, scope.ActorID, in.SOPID); err != nil {
			return empty, storeError(err)
		}
	}
	now := time.Now().UTC()
	t := domain.PersonalAgentTask{ID: newID("task"), ProjectID: scope.ProjectID, ActorID: scope.ActorID, Request: in.Request, Context: in.Context, SourceSOPID: in.SOPID, Status: "planning", Revision: 1, Plan: domain.PersonalAgentPlan{Agents: []domain.PersonalAgentSpec{}}, Runs: []domain.PersonalAgentRun{}, Feedback: []string{}, Events: []domain.PersonalAgentEvent{}, CreatedAt: now, UpdatedAt: now}
	addEvent(&t, "task.created", "任务已接收，Planner 正在根据需求设计角色", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return empty, fmt.Errorf("%w: service shutting down", ErrConflict)
	}
	if err := s.store.CreatePersonalAgentTask(t); err != nil {
		return empty, err
	}
	s.publishLocked(t)
	s.launchLocked(scope, t.ID, false)
	return t, nil
}

func (s *Service) Feedback(ctx context.Context, scope Scope, id string, in FeedbackRequest) (domain.PersonalAgentTask, error) {
	if err := scopeValid(scope); err != nil {
		return domain.PersonalAgentTask{}, err
	}
	if strings.TrimSpace(in.Message) == "" || len(in.Message) > 10000 {
		return domain.PersonalAgentTask{}, fmt.Errorf("%w: feedback required (max 10,000 bytes)", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.store.PersonalAgentTask(scope.ProjectID, scope.ActorID, id)
	if err != nil {
		return t, storeError(err)
	}
	if s.closed || t.Revision != in.Revision || t.ApprovedAt != nil || (t.Status != "awaiting_confirmation" && t.Status != "failed") {
		return t, ErrConflict
	}
	t.Feedback = append(t.Feedback, strings.TrimSpace(in.Message))
	t.Status = "planning"
	t.Error = ""
	t.PlanHash = ""
	t.SourceSOPID = ""
	t.UpdatedAt = time.Now().UTC()
	t.Revision++
	addEvent(&t, "task.feedback", "收到用户补充，Planner 将修订角色和任务计划", "")
	if err := s.store.UpdatePersonalAgentTask(t, in.Revision); err != nil {
		return t, storeError(err)
	}
	s.publishLocked(t)
	s.launchLocked(scope, t.ID, false)
	return t, nil
}

func (s *Service) Confirm(ctx context.Context, scope Scope, id string, in ConfirmRequest) (domain.PersonalAgentTask, error) {
	if err := scopeValid(scope); err != nil {
		return domain.PersonalAgentTask{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.store.PersonalAgentTask(scope.ProjectID, scope.ActorID, id)
	if err != nil {
		return t, storeError(err)
	}
	if s.closed || t.Status != "awaiting_confirmation" || t.Revision != in.Revision || t.PlanHash == "" || t.PlanHash != in.PlanHash {
		return t, ErrConflict
	}
	// Recompute from persisted evidence, never trust a client's hash alone.
	if hashPlan(t) != t.PlanHash {
		return t, ErrConflict
	}
	for _, a := range t.Plan.Agents {
		if a.Phase != "analysis" {
			continue
		}
		for _, run := range t.Runs {
			if run.AgentID != a.ID {
				continue
			}
			if output, ok := run.Output.(map[string]any); ok {
				if questions, ok := output["questions"].([]any); ok && len(questions) > 0 {
					return t, fmt.Errorf("%w: 请先补充回答分析中的待确认问题，再提交方案", ErrInvalid)
				}
			}
		}
	}
	now := time.Now().UTC()
	t.ApprovedAt = &now
	t.Status = "executing"
	t.UpdatedAt = now
	t.Revision++
	t.ApprovedRevision = t.Revision
	var sop *domain.PersonalAgentSOP
	if t.SourceSOPID != "" {
		t.SOPID = t.SourceSOPID
	} else {
		t.SOPID = newID("sop")
		sop = &domain.PersonalAgentSOP{ID: t.SOPID, ProjectID: t.ProjectID, ActorID: t.ActorID, Name: t.Plan.Name, Description: t.Plan.Description, MatchHints: t.Plan.MatchHints, Plan: t.Plan, Version: 1, SourceTaskID: t.ID, CreatedAt: now}
	}
	addEvent(&t, "task.confirmed", "用户已确认当前方案和分析证据，SOP 角色定义已冻结", "")
	if err := s.store.ConfirmPersonalAgentTask(t, in.Revision, sop); err != nil {
		return t, storeError(err)
	}
	s.publishLocked(t)
	s.launchLocked(scope, t.ID, true)
	return t, nil
}

func (s *Service) launchLocked(scope Scope, id string, execution bool) {
	worker := newID("worker")
	s.workers[id] = worker
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			if s.workers[id] == worker {
				delete(s.workers, id)
			}
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Minute)
		defer cancel()
		var err error
		if execution {
			err = s.execute(ctx, scope, id)
		} else {
			err = s.analyze(ctx, scope, id)
		}
		if err != nil {
			_ = s.change(scope, id, func(t *domain.PersonalAgentTask) error {
				t.Status = "failed"
				t.Error = err.Error()
				addEvent(t, "task.failed", err.Error(), "")
				return nil
			})
		}
	}()
}

func (s *Service) change(scope Scope, id string, fn func(*domain.PersonalAgentTask) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.store.PersonalAgentTask(scope.ProjectID, scope.ActorID, id)
	if err != nil {
		return storeError(err)
	}
	expected := t.Revision
	if err := fn(&t); err != nil {
		return err
	}
	t.Revision++
	t.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdatePersonalAgentTask(t, expected); err != nil {
		return storeError(err)
	}
	s.publishLocked(t)
	return nil
}

func (s *Service) analyze(ctx context.Context, scope Scope, id string) error {
	t, err := s.Get(ctx, scope, id)
	if err != nil {
		return err
	}
	tools, err := s.gateway.capabilities(ctx, scope)
	if err != nil {
		return err
	}
	var plan domain.PersonalAgentPlan
	source := t.SourceSOPID
	if source != "" {
		sop, err := s.store.PersonalAgentSOP(scope.ProjectID, scope.ActorID, source)
		if err != nil {
			return storeError(err)
		}
		if err := s.checkSOP(ctx, scope, t, sop, tools); err != nil {
			return err
		}
		plan = sop.Plan
	} else {
		sops, err := s.store.PersonalAgentSOPs(scope.ProjectID, scope.ActorID)
		if err != nil {
			return err
		}
		// Definitions are included for a grounded applicability decision. The
		// bounded newest list keeps planning context predictable in this MVP.
		if len(sops) > 20 {
			sops = sops[:20]
		}
		input := map[string]any{"request": t.Request, "context": t.Context, "feedback": t.Feedback, "priorPlan": t.Plan, "priorResults": t.Runs, "capabilities": tools, "availableSops": sops}
		content, err := s.gateway.model(ctx, scope, "planner", plannerPrompt, []Message{{Role: "user", Content: asJSON(input)}})
		if err != nil {
			return err
		}
		var choice struct {
			ReuseSOPID string `json:"reuseSopId"`
		}
		if err := decodeModel(content, &choice); err != nil {
			return fmt.Errorf("planner: %w", err)
		}
		if choice.ReuseSOPID != "" {
			sop, err := s.store.PersonalAgentSOP(scope.ProjectID, scope.ActorID, choice.ReuseSOPID)
			if err != nil {
				return fmt.Errorf("planner selected unavailable SOP: %w", storeError(err))
			}
			if err := s.checkSOP(ctx, scope, t, sop, tools); err != nil {
				return err
			}
			plan = sop.Plan
			source = sop.ID
		} else if err := decodeModel(content, &plan); err != nil {
			return fmt.Errorf("planner: %w", err)
		}
	}
	if err := validatePlan(plan, tools); err != nil {
		return fmt.Errorf("planner: %w", err)
	}
	if err := s.change(scope, id, func(t *domain.PersonalAgentTask) error {
		t.Plan = plan
		t.SourceSOPID = source
		t.Runs = make([]domain.PersonalAgentRun, 0, len(plan.Agents))
		t.Status = "analyzing"
		for _, a := range plan.Agents {
			t.Runs = append(t.Runs, domain.PersonalAgentRun{AgentID: a.ID, Status: "pending", ToolCalls: []domain.PersonalAgentToolCall{}})
		}
		message := fmt.Sprintf("Planner 已自主创建 %d 个角色", len(plan.Agents))
		if source != "" {
			message = "已加载 SOP 中冻结的角色定义，使用本次输入创建新实例"
		}
		addEvent(t, "plan.ready", message, "")
		return nil
	}); err != nil {
		return err
	}
	if err := s.runPhase(ctx, scope, id, "analysis", tools); err != nil {
		return err
	}
	return s.change(scope, id, func(t *domain.PersonalAgentTask) error {
		t.Status = "awaiting_confirmation"
		t.PlanHash = hashPlan(*t)
		addEvent(t, "confirmation.required", "分析已完成，请检查角色方案、疑问和业务变更明细后确认", "")
		return nil
	})
}
func (s *Service) checkSOP(ctx context.Context, scope Scope, t domain.PersonalAgentTask, sop domain.PersonalAgentSOP, tools []domain.PersonalAgentTool) error {
	content, err := s.gateway.model(ctx, scope, "sop_applicability", applicabilityPrompt, []Message{{Role: "user", Content: asJSON(map[string]any{"request": t.Request, "context": t.Context, "sop": sop, "capabilities": tools})}})
	if err != nil {
		return err
	}
	var result struct {
		Applicable bool   `json:"applicable"`
		Reason     string `json:"reason"`
	}
	if err := decodeModel(content, &result); err != nil {
		return err
	}
	if !result.Applicable {
		return fmt.Errorf("SOP 不适用于本次需求：%s", result.Reason)
	}
	return nil
}
func (s *Service) execute(ctx context.Context, scope Scope, id string) error {
	t, err := s.Get(ctx, scope, id)
	if err != nil {
		return err
	}
	if t.ApprovedAt == nil || hashPlan(t) != t.PlanHash {
		return ErrConflict
	}
	tools, err := s.gateway.capabilities(ctx, scope)
	if err != nil {
		return err
	}
	if err := validatePlan(t.Plan, tools); err != nil {
		return err
	}
	if err := s.runPhase(ctx, scope, id, "execution", tools); err != nil {
		return err
	}
	return s.change(scope, id, func(t *domain.PersonalAgentTask) error {
		t.Status = "succeeded"
		addEvent(t, "task.succeeded", "本次 SOP 已执行完成，角色与工具结果已保存", "")
		return nil
	})
}

func addEvent(t *domain.PersonalAgentTask, kind, message, agentID string) {
	t.Events = append(t.Events, domain.PersonalAgentEvent{ID: newID("event"), Type: kind, Message: message, AgentID: agentID, At: time.Now().UTC()})
}
func asJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}$`)

func validID(id string) bool { return identifier.MatchString(id) }
func decodeModel(content string, out any) error {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		if i := strings.Index(content, "\n"); i >= 0 {
			content = strings.TrimSpace(content[i+1:])
		}
		content = strings.TrimSuffix(content, "```")
	}
	if err := json.Unmarshal([]byte(content), out); err != nil {
		return fmt.Errorf("%w: model must return valid JSON", ErrGateway)
	}
	return nil
}
func hashPlan(t domain.PersonalAgentTask) string {
	analysis := []domain.PersonalAgentRun{}
	for _, a := range t.Plan.Agents {
		if a.Phase == "analysis" {
			for _, run := range t.Runs {
				if run.AgentID == a.ID {
					analysis = append(analysis, domain.PersonalAgentRun{AgentID: run.AgentID, Status: run.Status, Output: run.Output, ToolCalls: run.ToolCalls})
				}
			}
		}
	}
	sum := sha256.Sum256([]byte(asJSON(map[string]any{"request": t.Request, "context": t.Context, "feedback": t.Feedback, "plan": t.Plan, "analysis": analysis})))
	return hex.EncodeToString(sum[:])
}
