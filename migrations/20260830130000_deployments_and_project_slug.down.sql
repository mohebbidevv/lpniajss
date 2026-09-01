ALTER TABLE projects DROP COLUMN IF EXISTS current_deployment_id;

DROP INDEX IF EXISTS idx_projects_slug;
ALTER TABLE projects DROP COLUMN IF EXISTS slug;

DROP TABLE IF EXISTS deployments;
