-- Чеки «Мой налог»: по строке на платёж провайдера. Строка вставляется ДО
-- похода в ФНС — это и есть право на пробитие: не вставилась, значит чек уже
-- чей-то.
CREATE TABLE IF NOT EXISTS receipts (
    provider       TEXT NOT NULL,
    ext_id         TEXT NOT NULL,
    amount_kopecks BIGINT NOT NULL,
    name           TEXT NOT NULL,
    marker         TEXT NOT NULL,
    income_at      TEXT NOT NULL,
    receipt_uuid   TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    alerted_at     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider, ext_id)
)
