-- Optional manual baseline for the embedded Personal Agent service.
-- Apply with the normal deployment database account before starting the service.
-- No data is inserted or transformed. repo.Open may also add missing tables
-- and indexes through its existing incremental DDL path.

CREATE TABLE IF NOT EXISTS personal_agent_tasks (
    id VARCHAR(64) NOT NULL,
    project_id VARCHAR(128) NOT NULL,
    actor_id VARCHAR(128) NOT NULL,
    revision BIGINT NOT NULL,
    status VARCHAR(32) NOT NULL,
    payload JSON NOT NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_personal_tasks_scope (project_id, actor_id),
    KEY idx_personal_agent_tasks_status (status),
    KEY idx_personal_agent_tasks_updated (updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS personal_agent_sops (
    id VARCHAR(64) NOT NULL,
    project_id VARCHAR(128) NOT NULL,
    actor_id VARCHAR(128) NOT NULL,
    payload JSON NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_personal_sops_scope (project_id, actor_id),
    KEY idx_personal_agent_sops_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
