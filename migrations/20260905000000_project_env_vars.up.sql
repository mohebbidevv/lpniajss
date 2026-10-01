-- Per-project env vars, injected into the app's environment at deploy time
-- (see application.appEnv). Ownership is enforced entirely at the
-- application layer via the owning project, same as every other project
-- sub-resource — plaintext for MVP, per the deliberate "owner-only read is
-- sufficient for now" call; encryption-at-rest is a flagged fast-follow,
-- not implemented here.
CREATE TABLE project_env_vars (
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    key VARCHAR(255) NOT NULL,
    value TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (project_id, key)
);
