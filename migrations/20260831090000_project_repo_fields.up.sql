-- Tracks the origin repo for git-imported projects so DeployPipeline can
-- re-sync from it on every deploy (a "redeploy" is just running the pipeline
-- again). NULL for zip-uploaded projects.
ALTER TABLE projects ADD COLUMN repo_url TEXT NULL;
ALTER TABLE projects ADD COLUMN repo_ref VARCHAR(255) NULL;
