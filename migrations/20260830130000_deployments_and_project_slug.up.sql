-- Deployments: one row per build-and-start attempt for a project. A project
-- has many deployments over its life; projects.current_deployment_id (added
-- below) points at whichever one is (or was last) live.
CREATE TABLE deployments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    image_ref VARCHAR(512) NOT NULL DEFAULT '',
    container_id VARCHAR(255) NOT NULL DEFAULT '',
    status VARCHAR(50) NOT NULL DEFAULT 'building', -- building/running/stopped/failed/crashed
    port INTEGER NOT NULL DEFAULT 0,
    failure_reason TEXT NOT NULL DEFAULT '',
    exit_code INTEGER NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    started_at TIMESTAMP WITH TIME ZONE NULL,
    stopped_at TIMESTAMP WITH TIME ZONE NULL
);

CREATE INDEX idx_deployments_project_id ON deployments (project_id);
CREATE INDEX idx_deployments_status ON deployments (status);

-- projects.slug: the public subdomain, split out from unique_key. Existing
-- rows backfill from unique_key so the column can go NOT NULL UNIQUE.
ALTER TABLE projects ADD COLUMN slug VARCHAR(255);
UPDATE projects SET slug = unique_key WHERE slug IS NULL;
ALTER TABLE projects ALTER COLUMN slug SET NOT NULL;
CREATE UNIQUE INDEX idx_projects_slug ON projects (slug);

-- projects.current_deployment_id: which deployment is currently live.
-- Added after the deployments table so it can reference it; nullable
-- because a project that's never been deployed has none yet.
ALTER TABLE projects ADD COLUMN current_deployment_id UUID NULL
    REFERENCES deployments (id) ON DELETE SET NULL;
