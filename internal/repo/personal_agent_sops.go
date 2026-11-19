package repo

import (
	"agentflow/internal/domain"
	"encoding/json"
)

func (r *CatalogRepo) CreatePersonalAgentSOP(sop domain.PersonalAgentSOP) error {
	b, err := json.Marshal(sop)
	if err != nil {
		return err
	}
	return r.DB.Create(&personalAgentSOPRow{ID: sop.ID, ProjectID: sop.ProjectID, ActorID: sop.ActorID, Payload: b, Created: sop.CreatedAt}).Error
}
func (r *CatalogRepo) PersonalAgentSOP(projectID, actorID, id string) (domain.PersonalAgentSOP, error) {
	var row personalAgentSOPRow
	var sop domain.PersonalAgentSOP
	if err := r.DB.Where("id = ? AND project_id = ? AND actor_id = ?", id, projectID, actorID).Take(&row).Error; err != nil {
		return sop, err
	}
	err := json.Unmarshal(row.Payload, &sop)
	return sop, err
}
func (r *CatalogRepo) PersonalAgentSOPs(projectID, actorID string) ([]domain.PersonalAgentSOP, error) {
	var rows []personalAgentSOPRow
	if err := r.DB.Where("project_id = ? AND actor_id = ?", projectID, actorID).Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]domain.PersonalAgentSOP, 0, len(rows))
	for _, row := range rows {
		var sop domain.PersonalAgentSOP
		if err := json.Unmarshal(row.Payload, &sop); err != nil {
			return nil, err
		}
		out = append(out, sop)
	}
	return out, nil
}
