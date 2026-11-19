package personalagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// This gateway fixture controls model output, not application role definitions.
// Each task still creates real, isolated agent invocations and tool calls.
func TestPersonalAgentDynamicTeamApprovalAndSOPReuse(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{ProjectID: "tenant-a", ActorID: "operator-a"}
	var writes, plannerCalls, agentCalls atomic.Int32
	plan := domain.PersonalAgentPlan{Name: "Resource update", Description: "Inspect and apply current input", MatchHints: []string{"resource update"}, Agents: []domain.PersonalAgentSpec{
		{ID: "investigator-generated", Name: "Generated investigator", Purpose: "Inspect this request", Instructions: "Inspect CURRENT task inputs and propose changes", Tools: []string{"inspect_resource"}, DependsOn: []string{}, Phase: "analysis", OutputContract: "summary, questions, proposal reference"},
		{ID: "executor-generated", Name: "Generated executor", Purpose: "Apply approved proposal", Instructions: "Apply proposal from approved analysis", Tools: []string{"apply_plan"}, DependsOn: []string{"investigator-generated"}, Phase: "execution", OutputContract: "summary, result"},
	}}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Project-ID") != scope.ProjectID || r.Header.Get("X-Actor-ID") != scope.ActorID {
			t.Error("gateway scope missing")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/capabilities":
			json.NewEncoder(w).Encode(map[string]any{"tools": []domain.PersonalAgentTool{{Name: "inspect_resource", Description: "Preview current requested resource changes", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}, Effect: "read", RequiredPhase: "analysis"}, {Name: "apply_plan", Description: "Apply an approved proposal", InputSchema: map[string]any{"type": "object", "required": []any{"proposalId"}, "properties": map[string]any{"proposalId": map[string]any{"type": "string"}}}, Effect: "write", RequiredPhase: "execution"}}})
		case "/model":
			var in struct {
				Purpose  string    `json:"purpose"`
				Messages []Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
				return
			}
			var out any
			if in.Purpose == "planner" {
				plannerCalls.Add(1)
				out = plan
			} else if in.Purpose == "sop_applicability" {
				out = map[string]any{"applicable": true, "reason": "same parameterized action"}
			} else {
				agentCalls.Add(1)
				if len(in.Messages) == 1 {
					var input struct {
						TaskID string `json:"taskId"`
					}
					json.Unmarshal([]byte(in.Messages[0].Content), &input)
					if strings.Contains(in.Purpose, "investigator") {
						out = map[string]any{"action": "tool", "tool": "inspect_resource", "arguments": map[string]any{}}
					} else {
						out = map[string]any{"action": "tool", "tool": "apply_plan", "arguments": map[string]any{"proposalId": "proposal-" + input.TaskID}}
					}
				} else {
					out = map[string]any{"action": "complete", "output": map[string]any{"summary": "completed with tool evidence", "questions": []any{}}}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"content": asJSON(out)})
		case "/tools/inspect_resource":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"proposalId": "proposal-" + in["taskId"].(string), "items": []any{"current-input"}}})
		case "/tools/apply_plan":
			var in struct {
				TaskID   string `json:"taskId"`
				Approval struct {
					Revision int    `json:"revision"`
					PlanHash string `json:"planHash"`
				} `json:"approval"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			task, err := store.PersonalAgentTask(scope.ProjectID, scope.ActorID, in.TaskID)
			if err != nil || task.ApprovedAt == nil || task.ApprovedRevision != in.Approval.Revision || task.PlanHash != in.Approval.PlanHash {
				t.Error("write not bound to persisted approval")
				w.WriteHeader(403)
				return
			}
			writes.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"applied": true}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer gateway.Close()
	client, err := NewHTTPGateway(gateway.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, client)
	defer service.Close()
	task, err := service.Create(context.Background(), scope, CreateRequest{Request: "Update resource", Context: map[string]any{"inputId": "first"}})
	if err != nil {
		t.Fatal(err)
	}
	task = waitTask(t, service, scope, task.ID, "awaiting_confirmation")
	if writes.Load() != 0 {
		t.Fatal("write occurred before confirmation")
	}
	if len(task.Plan.Agents) != 2 || task.Plan.Agents[0].ID != "investigator-generated" {
		t.Fatal("planner generated definitions were not instantiated")
	}
	if _, err := service.Get(context.Background(), Scope{ProjectID: scope.ProjectID, ActorID: "other"}, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scope leaked: %v", err)
	}
	if _, err := service.Confirm(context.Background(), scope, task.ID, ConfirmRequest{Revision: task.Revision - 1, PlanHash: task.PlanHash}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale confirmation: %v", err)
	}
	if _, err := service.Confirm(context.Background(), scope, task.ID, ConfirmRequest{Revision: task.Revision, PlanHash: "tampered"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("tampered confirmation: %v", err)
	}
	if _, err := service.Confirm(context.Background(), scope, task.ID, ConfirmRequest{Revision: task.Revision, PlanHash: task.PlanHash}); err != nil {
		t.Fatal(err)
	}
	task = waitTask(t, service, scope, task.ID, "succeeded")
	if writes.Load() != 1 || task.SOPID == "" {
		t.Fatal("confirmed execution/SOP save missing")
	}
	sops, err := service.SOPs(context.Background(), scope)
	if err != nil || len(sops) != 1 {
		t.Fatalf("SOPs: %v %v", sops, err)
	}
	if len(sops[0].Plan.Agents) != 2 || strings.Contains(asJSON(sops[0].Plan), "proposal-task-") {
		t.Fatal("SOP should preserve definitions, not old execution evidence")
	}
	second, err := service.Create(context.Background(), scope, CreateRequest{Request: "Update another resource", Context: map[string]any{"inputId": "second"}, SOPID: task.SOPID})
	if err != nil {
		t.Fatal(err)
	}
	second = waitTask(t, service, scope, second.ID, "awaiting_confirmation")
	if plannerCalls.Load() != 1 {
		t.Fatal("SOP reuse recreated role definitions")
	}
	if second.Runs[0].ToolCalls[0].Result.(map[string]any)["proposalId"] == task.Runs[0].ToolCalls[0].Result.(map[string]any)["proposalId"] {
		t.Fatal("SOP reuse leaked prior task proposal")
	}
	if writes.Load() != 1 {
		t.Fatal("SOP reuse bypassed new business confirmation")
	}
	if agentCalls.Load() < 6 {
		t.Fatal("independent agent invocations missing")
	}
}

func waitTask(t *testing.T, s *Service, scope Scope, id, status string) domain.PersonalAgentTask {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task, err := s.Get(context.Background(), scope, id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status == status {
			return task
		}
		if task.Status == "failed" {
			t.Fatalf("task failed: %s", task.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not reach " + status)
	return domain.PersonalAgentTask{}
}

func TestPersonalAgentRejectsPrivilegeEscalationAndCycles(t *testing.T) {
	tools := []domain.PersonalAgentTool{{Name: "write_record", Effect: "write"}}
	plan := domain.PersonalAgentPlan{Name: "n", Description: "d", Agents: []domain.PersonalAgentSpec{{ID: "a", Name: "a", Purpose: "p", Instructions: "i", OutputContract: "o", Phase: "analysis", Tools: []string{"write_record"}}}}
	if err := validatePlan(plan, tools); !errors.Is(err, ErrInvalid) {
		t.Fatal("analysis was allowed write privilege")
	}
	plan.Agents[0].Tools = nil
	plan.Agents[0].DependsOn = []string{"b"}
	plan.Agents = append(plan.Agents, domain.PersonalAgentSpec{ID: "b", Name: "b", Purpose: "p", Instructions: "i", OutputContract: "o", Phase: "analysis", DependsOn: []string{"a"}})
	if err := validatePlan(plan, tools); !errors.Is(err, ErrInvalid) {
		t.Fatal("cyclic planner output accepted")
	}
}

func TestPersonalAgentCrashRecoveryMarksUncertainWritesWithoutReplay(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	task := domain.PersonalAgentTask{ID: "interrupted-task", ProjectID: "p", ActorID: "a", Status: "executing", Revision: 4, CreatedAt: now, UpdatedAt: now, Runs: []domain.PersonalAgentRun{{AgentID: "writer", Status: "running", ToolCalls: []domain.PersonalAgentToolCall{{ID: "write-1", Name: "write_resource", Effect: "write", Status: "running"}}}}}
	if err := store.CreatePersonalAgentTask(task); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil)
	defer service.Close()
	if err := service.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Get(context.Background(), Scope{ProjectID: "p", ActorID: "a"}, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "failed" || recovered.Runs[0].ToolCalls[0].Status != "unknown" || recovered.Revision != 5 {
		t.Fatalf("unsafe interrupted state: %#v", recovered)
	}
	if err := service.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	again, _ := service.Get(context.Background(), Scope{ProjectID: "p", ActorID: "a"}, task.ID)
	if again.Revision != 5 {
		t.Fatal("recovery should not replay or mutate already settled task")
	}
}

func TestPersonalAgentConfirmationRequiresResolvedQuestions(t *testing.T) {
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{ProjectID: "p", ActorID: "a"}
	now := time.Now().UTC()
	task := domain.PersonalAgentTask{ID: "needs-input", ProjectID: scope.ProjectID, ActorID: scope.ActorID, Request: "Act on resources", Status: "awaiting_confirmation", Revision: 1, CreatedAt: now, UpdatedAt: now, Plan: domain.PersonalAgentPlan{Name: "p", Agents: []domain.PersonalAgentSpec{{ID: "analyst", Phase: "analysis"}}}, Runs: []domain.PersonalAgentRun{{AgentID: "analyst", Status: "succeeded", Output: map[string]any{"summary": "ambiguous target", "questions": []any{"Which scope?"}}}}}
	task.PlanHash = hashPlan(task)
	if err := store.CreatePersonalAgentTask(task); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil)
	defer service.Close()
	if _, err := service.Confirm(context.Background(), scope, task.ID, ConfirmRequest{Revision: task.Revision, PlanHash: task.PlanHash}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unresolved questions were approved: %v", err)
	}
	if sops, err := service.SOPs(context.Background(), scope); err != nil || len(sops) != 0 {
		t.Fatal("invalid confirmation published SOP")
	}
}

func TestPersonalAgentRequiredToolsCannotBeSkippedByModelCompletion(t *testing.T) {
	for _, effect := range []string{"read", "write"} {
		t.Run(effect, func(t *testing.T) {
			store, err := repo.OpenTest()
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			name := "required_" + effect
			plan := domain.PersonalAgentPlan{Name: "host task", Description: "requires real evidence", Agents: []domain.PersonalAgentSpec{
				{ID: "analysis", Name: "Analysis", Purpose: "analyze", Instructions: "analyze", OutputContract: "summary", Phase: "analysis"},
				{ID: "execution", Name: "Execution", Purpose: "execute", Instructions: "execute and verify", OutputContract: "summary", Phase: "execution", DependsOn: []string{"analysis"}, Tools: []string{name}},
			}}
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/capabilities":
					json.NewEncoder(w).Encode(map[string]any{"tools": []domain.PersonalAgentTool{{Name: name, Description: "host-required evidence", Effect: effect, RequiredPhase: "execution", InputSchema: map[string]any{"type": "object"}}}})
				case "/model":
					var in struct {
						Purpose string `json:"purpose"`
					}
					json.NewDecoder(r.Body).Decode(&in)
					var out any = map[string]any{"action": "complete", "output": map[string]any{"summary": "model claims success without tool evidence", "questions": []any{}}}
					if in.Purpose == "planner" {
						out = plan
					}
					json.NewEncoder(w).Encode(map[string]any{"content": asJSON(out)})
				default:
					t.Error("model fixture should not call tools")
					w.WriteHeader(500)
				}
			}))
			defer gateway.Close()
			client, _ := NewHTTPGateway(gateway.URL, "token")
			service := NewService(store, client)
			defer service.Close()
			scope := Scope{ProjectID: "p", ActorID: "a"}
			task, err := service.Create(context.Background(), scope, CreateRequest{Request: "perform host operation"})
			if err != nil {
				t.Fatal(err)
			}
			task = waitTask(t, service, scope, task.ID, "awaiting_confirmation")
			if _, err := service.Confirm(context.Background(), scope, task.ID, ConfirmRequest{Revision: task.Revision, PlanHash: task.PlanHash}); err != nil {
				t.Fatal(err)
			}
			task = waitTask(t, service, scope, task.ID, "failed")
			if !strings.Contains(task.Error, name) {
				t.Fatalf("missing evidence was not enforced: %s", task.Error)
			}
		})
	}
}

func TestPersonalAgentRequiredPhaseAssignmentAndUnresolvedAnalysis(t *testing.T) {
	tools := []domain.PersonalAgentTool{{Name: "preview", Effect: "read", RequiredPhase: "analysis"}, {Name: "apply", Effect: "write", RequiredPhase: "execution"}, {Name: "verify", Effect: "read", RequiredPhase: "execution"}}
	plan := domain.PersonalAgentPlan{Name: "n", Description: "d", Agents: []domain.PersonalAgentSpec{{ID: "a", Name: "a", Purpose: "p", Instructions: "i", OutputContract: "o", Phase: "analysis", Tools: []string{"preview"}}}}
	if err := validatePlan(plan, tools); !errors.Is(err, ErrInvalid) {
		t.Fatal("analysis-only plan omitted required execution tools")
	}
	task := domain.PersonalAgentTask{Plan: plan, Runs: []domain.PersonalAgentRun{{AgentID: "a", Status: "succeeded", Output: map[string]any{"questions": []any{"Which target?"}}}}}
	if err := requiredPhaseEvidence(task, "analysis", tools); err != nil {
		t.Fatal("unresolved analysis must pause for user input", err)
	}
	task.Runs[0].Output = map[string]any{"questions": []any{}}
	if err := requiredPhaseEvidence(task, "analysis", tools); err == nil {
		t.Fatal("model completion bypassed required preview")
	}
	task.Runs[0].ToolCalls = []domain.PersonalAgentToolCall{{Name: "preview", Status: "succeeded"}}
	if err := requiredPhaseEvidence(task, "analysis", tools); err != nil {
		t.Fatal(err)
	}
}
