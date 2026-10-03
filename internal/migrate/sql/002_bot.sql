-- Бот: карточка в TG, на которую можно ответить уточнением, и причина, если разбор не удался.
ALTER TABLE inbox_messages
    ADD COLUMN bot_message_id bigint,
    ADD COLUMN parse_error    text;

CREATE INDEX inbox_bot_msg_idx ON inbox_messages (bot_message_id) WHERE bot_message_id IS NOT NULL;
