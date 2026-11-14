-- Автор приглашения: 0 — владелец, иначе Telegram ID пользователя.
-- Существующие приглашения — владельца.
ALTER TABLE invites ADD COLUMN IF NOT EXISTS created_by BIGINT NOT NULL DEFAULT 0;
