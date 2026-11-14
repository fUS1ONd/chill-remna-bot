-- Момент списания денег по данным провайдера (ЮKassa: captured_at); '' —
-- неизвестен. Им датируется чек «Мой налог»: бот мог узнать об оплате позже.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS captured_at TEXT NOT NULL DEFAULT '';
