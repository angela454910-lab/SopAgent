package personalagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agentflow/internal/domain"
)

func validatePlan(plan domain.PersonalAgentPlan, tools []domain.PersonalAgentTool) error {
	if plan.Name == "" || plan.Description == "" || len(plan.Agents) < 1 || len(plan.Agents) > 8 {
		return fmt.Errorf("%w: a named plan needs 1-8 agents", ErrInvalid)
	}
	capabilities := map[string]domain.PersonalAgentTool{}
	for _, tool := range tools {
		capabilities[tool.Name] = tool
	}
	agents := map[string]domain.PersonalAgentSpec{}
	analysis := false
	for _, a := range plan.Agents {
		if !validID(a.ID) || a.Name == "" || a.Purpose == "" || a.Instructions == "" || a.OutputContract == "" || len(a.Instructions) > 16000 {
			return fmt.Errorf("%w: each generated role needs id, name, purpose, instructions and outputContract", ErrInvalid)
		}
		if _, ok := agents[a.ID]; ok {
			return fmt.Errorf("%w: duplicate agent ID", ErrInvalid)
		}
		if a.Phase != "analysis" && a.Phase != "execution" {
			return fmt.Errorf("%w: invalid agent phase", ErrInvalid)
		}
		if a.Phase == "analysis" {
			analysis = true
		}
		for _, name := range a.Tools {
			tool, ok := capabilities[name]
			if !ok {
				return fmt.Errorf("%w: role %s requests unavailable tool %s", ErrInvalid, a.Name, name)
			}
			if a.Phase == "analysis" && tool.Effect != "read" {
				return fmt.Errorf("%w: analysis role requests write tool", ErrInvalid)
			}
		}
		agents[a.ID] = a
	}
	if !analysis {
		return fmt.Errorf("%w: analysis phase is required", ErrInvalid)
	}
	for _, tool := range tools {
		if tool.RequiredPhase == "" {
			continue
		}
		assigned := false
		for _, a := range plan.Agents {
			if a.Phase != tool.RequiredPhase {
				continue
			}
			for _, name := range a.Tools {
				if name == tool.Name {
					assigned = true
				}
			}
		}
		if !assigned {
			return fmt.Errorf("%w: host requires tool %s assigned in %s phase", ErrInvalid, tool.Name, tool.RequiredPhase)
		}
	}
	for _, a := range plan.Agents {
		for _, dep := range a.DependsOn {
			d, ok := agents[dep]
			if !ok || dep == a.ID {
				return fmt.Errorf("%w: invalid dependency", ErrInvalid)
			}
			if a.Phase == "analysis" && d.Phase != "analysis" {
				return fmt.Errorf("%w: analysis cannot depend on execution", ErrInvalid)
			}
		}
	}
	visited := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if visited[id] == 1 {
			return fmt.Errorf("%w: cyclic role dependencies", ErrInvalid)
		}
		if visited[id] == 2 {
			return nil
		}
		visited[id] = 1
		for _, dep := range agents[id].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[id] = 2
		return nil
	}
	for id := range agents {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) runPhase(ctx context.Context, scope Scope, id, phase string, tools []domain.PersonalAgentTool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t, err := s.Get(ctx, scope, id)
		if err != nil {
			return err
		}
		states := map[string]string{}
		for _, run := range t.Runs {
			states[run.AgentID] = run.Status
		}
		ready := []domain.PersonalAgentSpec{}
		remaining := 0
		for _, a := range t.Plan.Agents {
			if a.Phase != phase || states[a.ID] == "succeeded" {
				continue
			}
			remaining++
			if states[a.ID] != "pending" {
				return fmt.Errorf("角色 %s 未正常完成，禁止自动重放可能产生副作用的步骤", a.Name)
			}
			all := true
			for _, dep := range a.DependsOn {
				if states[dep] != "succeeded" {
					all = false
					break
				}
			}
			if all && len(ready) < 2 {
				ready = append(ready, a)
			}
		}
		if remaining == 0 {
			return requiredPhaseEvidence(t, phase, tools)
		}
		if len(ready) == 0 {
			return fmt.Errorf("%w: no runnable agent", ErrInvalid)
		}
		phaseCtx, cancel := context.WithCancel(ctx)
		errs := make(chan error, len(ready))
		var wg sync.WaitGroup
		for _, agent := range ready {
			wg.Add(1)
			go func(a domain.PersonalAgentSpec) {
				defer wg.Done()
				if err := s.runAgent(phaseCtx, scope, id, a, tools); err != nil {
					cancel()
					errs <- err
				}
			}(agent)
		}
		wg.Wait()
		cancel()
		close(errs)
		for err := range errs {
			if err != nil {
				return err
			}
		}
	}
}

// Completion is determined by trusted host evidence requirements, not a model's
// complete action. Analysis may pause with unresolved questions; Confirm blocks
// that draft until the user answers and the task has been replanned.
func requiredPhaseEvidence(t domain.PersonalAgentTask, phase string, tools []domain.PersonalAgentTool) error {
	phaseAgents := map[string]bool{}
	for _, a := range t.Plan.Agents {
		if a.Phase == phase {
			phaseAgents[a.ID] = true
		}
	}
	if phase == "analysis" {
		for _, run := range t.Runs {
			if !phaseAgents[run.AgentID] {
				continue
			}
			if output, ok := run.Output.(map[string]any); ok {
				if questions, ok := output["questions"].([]any); ok && len(questions) > 0 {
					return nil
				}
			}
		}
	}
	succeeded := map[string]bool{}
	for _, run := range t.Runs {
		if !phaseAgents[run.AgentID] || run.Status != "succeeded" {
			continue
		}
		for _, call := range run.ToolCalls {
			if call.Status == "succeeded" {
				succeeded[call.Name] = true
			}
		}
	}
	for _, tool := range tools {
		if tool.RequiredPhase == phase && !succeeded[tool.Name] {
			return fmt.Errorf("宿主要求的 %s 阶段工具 %s 没有成功调用证据，不能将任务标记为完成", phase, tool.Name)
		}
	}
	return nil
}

func (s *Service) runAgent(ctx context.Context, scope Scope, id string, a domain.PersonalAgentSpec, tools []domain.PersonalAgentTool) (runErr error) {
	if err := s.change(scope, id, func(t *domain.PersonalAgentTask) error {
		run := findRun(t, a.ID)
		now := time.Now().UTC()
		run.Status = "running"
		run.StartedAt = &now
		addEvent(t, "agent.started", a.Name+" 开始执行", a.ID)
		return nil
	}); err != nil {
		return err
	}
	defer func() {
		if runErr != nil {
			_ = s.change(scope, id, func(t *domain.PersonalAgentTask) error {
				run := findRun(t, a.ID)
				now := time.Now().UTC()
				run.Status = "failed"
				run.Error = runErr.Error()
				run.FinishedAt = &now
				addEvent(t, "agent.failed", a.Name+"："+runErr.Error(), a.ID)
				return nil
			})
		}
	}()
	t, err := s.Get(ctx, scope, id)
	if err != nil {
		return err
	}
	allowed := map[string]domain.PersonalAgentTool{}
	available := []domain.PersonalAgentTool{}
	for _, name := range a.Tools {
		for _, tool := range tools {
			if tool.Name == name {
				allowed[name] = tool
				available = append(available, tool)
			}
		}
	}
	deps := map[string]any{}
	approved := map[string]any{}
	for _, run := range t.Runs {
		for _, dep := range a.DependsOn {
			if run.AgentID == dep {
				deps[dep] = run
			}
		}
		if a.Phase == "execution" {
			for _, spec := range t.Plan.Agents {
				if spec.ID == run.AgentID && spec.Phase == "analysis" {
					approved[run.AgentID] = run
				}
			}
		}
	}
	input := map[string]any{"taskId": id, "request": t.Request, "context": t.Context, "feedback": t.Feedback, "role": a, "dependencies": deps, "approvedAnalysis": approved, "tools": available, "toolBudget": 6}
	messages := []Message{{Role: "user", Content: asJSON(input)}}
	for turn := 0; turn <= 6; turn++ {
		content, err := s.gateway.model(ctx, scope, "agent:"+a.ID, agentPrompt, messages)
		if err != nil {
			return err
		}
		var action struct {
			Action    string         `json:"action"`
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
			Output    any            `json:"output"`
		}
		if err := decodeModel(content, &action); err != nil {
			return err
		}
		if action.Action == "complete" {
			if action.Output == nil || len(asJSON(action.Output)) > 128*1024 {
				return fmt.Errorf("%w: agent output missing or exceeds 128 KiB", ErrGateway)
			}
			return s.change(scope, id, func(t *domain.PersonalAgentTask) error {
				run := findRun(t, a.ID)
				now := time.Now().UTC()
				run.Status = "succeeded"
				run.Output = action.Output
				run.FinishedAt = &now
				addEvent(t, "agent.completed", a.Name+" 已完成", a.ID)
				return nil
			})
		}
		if action.Action != "tool" || turn == 6 {
			return fmt.Errorf("%w: invalid action or tool budget exhausted", ErrGateway)
		}
		tool, ok := allowed[action.Tool]
		if !ok {
			return fmt.Errorf("%w: role attempted ungranted tool %s", ErrInvalid, action.Tool)
		}
		if err := validateArguments(action.Arguments, tool.InputSchema, 0); err != nil {
			return fmt.Errorf("%w: %s arguments: %v", ErrInvalid, tool.Name, err)
		}
		if tool.Effect == "write" && (a.Phase != "execution" || t.ApprovedAt == nil || t.ApprovedRevision == 0 || hashPlan(t) != t.PlanHash) {
			return fmt.Errorf("%w: write requires approval of this exact plan and evidence", ErrConflict)
		}
		callID := newID("call")
		if err := s.change(scope, id, func(t *domain.PersonalAgentTask) error {
			run := findRun(t, a.ID)
			run.ToolCalls = append(run.ToolCalls, domain.PersonalAgentToolCall{ID: callID, Name: tool.Name, Arguments: action.Arguments, Effect: tool.Effect, Status: "running"})
			addEvent(t, "tool.started", a.Name+" 调用 "+tool.Name, a.ID)
			return nil
		}); err != nil {
			return err
		}
		args := map[string]any{"taskId": id, "callId": callID, "arguments": action.Arguments}
		if a.Phase == "execution" {
			args["approval"] = map[string]any{"taskId": id, "revision": t.ApprovedRevision, "planHash": t.PlanHash}
		}
		result, toolErr := s.gateway.tool(ctx, scope, tool.Name, args)
		if len(asJSON(result)) > 128*1024 {
			toolErr = fmt.Errorf("tool response exceeds 128 KiB")
		}
		if err := s.change(scope, id, func(t *domain.PersonalAgentTask) error {
			run := findRun(t, a.ID)
			for i := range run.ToolCalls {
				if run.ToolCalls[i].ID != callID {
					continue
				}
				call := &run.ToolCalls[i]
				if toolErr != nil {
					call.Status = "failed"
					if tool.Effect == "write" {
						call.Status = "unknown"
					}
					call.Error = toolErr.Error()
				} else {
					call.Status = "succeeded"
					call.Result = result
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if toolErr != nil {
			if tool.Effect == "write" {
				return fmt.Errorf("%s 结果未知，需核对外部状态；系统不会自动重试：%w", tool.Name, toolErr)
			}
			return toolErr
		}
		messages = append(messages, Message{Role: "assistant", Content: content}, Message{Role: "user", Content: asJSON(map[string]any{"tool": tool.Name, "callId": callID, "result": result, "toolCallsRemaining": 5 - turn})})
	}
	return fmt.Errorf("tool budget exhausted")
}

func findRun(t *domain.PersonalAgentTask, id string) *domain.PersonalAgentRun {
	for i := range t.Runs {
		if t.Runs[i].AgentID == id {
			return &t.Runs[i]
		}
	}
	panic("validated role missing execution state")
}

// validateArguments checks the common structural JSON-schema subset at the
// generic boundary. Host tools must additionally validate business semantics.
func validateArguments(value any, schema map[string]any, depth int) error {
	if depth > 12 {
		return fmt.Errorf("schema nesting exceeds limit")
	}
	if enums, ok := schema["enum"].([]any); ok {
		match := false
		for _, v := range enums {
			if asJSON(value) == asJSON(v) {
				match = true
			}
		}
		if !match {
			return fmt.Errorf("value not in enum")
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		if required, ok := schema["required"].([]any); ok {
			for _, key := range required {
				k, _ := key.(string)
				if _, exists := obj[k]; !exists {
					return fmt.Errorf("missing %s", k)
				}
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, v := range obj {
			p, ok := properties[key].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("unexpected field %s", key)
				}
				continue
			}
			if err := validateArguments(v, p, depth+1); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected array")
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for _, v := range array {
				if err := validateArguments(v, items, depth+1); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("expected string")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	case "number", "integer":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("expected number")
		}
	}
	return nil
}
