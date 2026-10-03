-- Дневник: ответ на бриф в Telegram дописывается в дневник — нужен id сообщения брифа.
ALTER TABLE briefs ADD COLUMN tg_message_id bigint;
CREATE INDEX briefs_tg_msg_idx ON briefs (tg_message_id) WHERE tg_message_id IS NOT NULL;

-- Идемпотентность записей из офлайн-очереди: повтор с тем же ключом не выполняется.
CREATE TABLE idempotency_keys (
    key        text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);
