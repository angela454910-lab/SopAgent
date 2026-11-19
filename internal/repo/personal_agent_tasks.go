package repo

import (
	"agentflow/internal/domain"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

func (r *RunsRepo) CreatePersonalAgentTask(task domain.PersonalAgentTask) error {
	b, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return r.DB.Create(&personalAgentTaskRow{ID: task.ID, ProjectID: task.ProjectID, ActorID: task.ActorID, Revision: task.Revision, Status: task.Status, Payload: b, Updated: task.UpdatedAt}).Error
}

func (r *RunsRepo) PersonalAgentTask(projectID, actorID, id string) (domain.PersonalAgentTask, error) {
	var row personalAgentTaskRow
	var task domain.PersonalAgentTask
	err := r.DB.Where("id = ? AND project_id = ? AND actor_id = ?", id, projectID, actorID).Take(&row).Error
	if err != nil {
		return task, err
	}
	err = json.Unmarshal(row.Payload, &task)
	return task, err
}

func (r *RunsRepo) PersonalAgentTasks(projectID, actorID string) ([]domain.PersonalAgentTask, error) {
	var rows []personalAgentTaskRow
	if err := r.DB.Where("project_id = ? AND actor_id = ?", projectID, actorID).Order("updated_at DESC").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.PersonalAgentTask, 0, len(rows))
	for _, row := range rows {
		var t domain.PersonalAgentTask
		if err := json.Unmarshal(row.Payload, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// UpdatePersonalAgentTask fences stale confirmations, feedback and workers.
func (r *RunsRepo) UpdatePersonalAgentTask(task domain.PersonalAgentTask, expected int) error {
	if task.Revision != expected+1 {
		return errors.New("personal agent task revision must increment once")
	}
	b, err := json.Marshal(task)
	if err != nil {
		return err
	}
	result := r.DB.Model(&personalAgentTaskRow{}).Where("id = ? AND project_id = ? AND actor_id = ? AND revision = ?", task.ID, task.ProjectID, task.ActorID, expected).Updates(map[string]any{"payload": b, "revision": task.Revision, "status": task.Status, "updated_at": task.UpdatedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRunDecisionStale
	}
	return nil
}

func IsPersonalAgentNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

// ActivePersonalAgentTasks is only for explicit single-worker recovery before
// the headless service starts accepting requests.
func (r *RunsRepo) ActivePersonalAgentTasks() ([]domain.PersonalAgentTask, error) {
	var rows []personalAgentTaskRow
	if err := r.DB.Where("status IN ?", []string{"planning", "analyzing", "executing"}).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.PersonalAgentTask, 0, len(rows))
	for _, row := range rows {
		var task domain.PersonalAgentTask
		if err := json.Unmarshal(row.Payload, &task); err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}

// ConfirmPersonalAgentTask publishes the immutable catalog snapshot and the
// exact task approval atomically. Catalog collaboration is explicitly injected
// through RunsRepo.catalog in newStore.
func (r *RunsRepo) ConfirmPersonalAgentTask(task domain.PersonalAgentTask, expected int, sop *domain.PersonalAgentSOP) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if sop != nil {
			catalog := *r.catalog
			catalog.deps = &deps{DB: tx}
			if err := catalog.CreatePersonalAgentSOP(*sop); err != nil {
				return err
			}
		}
		runs := *r
		runs.deps = &deps{DB: tx}
		return runs.UpdatePersonalAgentTask(task, expected)
	})
}
