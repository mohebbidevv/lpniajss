-- Email verification and password reset share one primitive: a single-use,
-- expiring, hashed token.
CREATE TABLE IF NOT EXISTS user_tokens (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- purpose is not optional: without it a password-reset token could be
    -- redeemed as an email verification and vice versa. Never make a token
    -- type-agnostic.
    purpose    TEXT NOT NULL CHECK (purpose IN ('email_verify', 'password_reset')),

    -- SHA-256 of the token, never the token. A leaked database must not hand
    -- over working reset links — the same reasoning as the sessions table.
    -- SHA-256 rather than bcrypt is correct here: these are 32 bytes of
    -- crypto/rand, so there is no dictionary to attack and no need for a
    -- deliberately slow hash.
    token_hash TEXT NOT NULL UNIQUE,

    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_tokens_hash ON user_tokens (token_hash);
CREATE INDEX IF NOT EXISTS idx_user_tokens_user_purpose ON user_tokens (user_id, purpose);

-- Existing accounts are grandfathered in as verified: introducing the gate
-- must not lock out everyone who signed up before it existed.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE users ALTER COLUMN email_verified SET DEFAULT FALSE;
