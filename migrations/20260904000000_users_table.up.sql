CREATE TABLE IF NOT EXISTS users  (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid (),
    email VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash VARCHAR(255) NOT NULL UNIQUE,
    user_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), 
    expires_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT fk_session_user
        FOREIGN KEY(user_id)
        REFERENCES users(id)
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);

ALTER TABLE projects ADD COLUMN user_id UUID REFERENCES users(id);

CREATE INDEX idx_projects_user_id ON projects (user_id);