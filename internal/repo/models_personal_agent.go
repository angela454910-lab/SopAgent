package repo

import "time"

type personalAgentTaskRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;size:128;index:idx_personal_tasks_scope,priority:1"`
	ActorID   string    `gorm:"column:actor_id;size:128;index:idx_personal_tasks_scope,priority:2"`
	Revision  int       `gorm:"column:revision"`
	Status    string    `gorm:"column:status;size:32;index"`
	Payload   []byte    `gorm:"column:payload;type:json"`
	Updated   time.Time `gorm:"column:updated_at;index"`
}

func (personalAgentTaskRow) TableName() string { return "personal_agent_tasks" }

type personalAgentSOPRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	ProjectID string    `gorm:"column:project_id;size:128;index:idx_personal_sops_scope,priority:1"`
	ActorID   string    `gorm:"column:actor_id;size:128;index:idx_personal_sops_scope,priority:2"`
	Payload   []byte    `gorm:"column:payload;type:json"`
	Created   time.Time `gorm:"column:created_at;index"`
}

func (personalAgentSOPRow) TableName() string { return "personal_agent_sops" }
