-- Что выдаёт приглашение; существующие — только Доступ.
ALTER TABLE invites ADD COLUMN grant_kind TEXT NOT NULL DEFAULT 'access';
