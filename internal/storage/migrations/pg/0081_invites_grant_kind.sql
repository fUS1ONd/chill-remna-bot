-- Что выдаёт приглашение; существующие — только Доступ.
ALTER TABLE invites ADD COLUMN IF NOT EXISTS grant_kind TEXT NOT NULL DEFAULT 'access';
