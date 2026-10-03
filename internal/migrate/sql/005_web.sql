-- Веб-вход: одноразовая ссылка из бота (/login) → сессия в cookie.
-- Храним только sha256 от токенов: утечка БД не даёт войти.
CREATE TABLE login_tokens (
    token_hash text PRIMARY KEY,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);

CREATE TABLE sessions (
    id_hash    text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    user_agent text NOT NULL DEFAULT ''
);
