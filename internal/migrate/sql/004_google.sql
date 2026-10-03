-- Синхронизация с Google: служебные значения (refresh-токен, id календаря),
-- метка последней выгрузки задачи и «надгробия» удалённых событий.
CREATE TABLE kv (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Задача выгружена в Google в состоянии updated_at = gcal_synced_at; если
-- updated_at ушёл вперёд — её надо выгрузить снова.
ALTER TABLE items ADD COLUMN gcal_synced_at timestamptz;

-- Задачу удалили (в т.ч. каскадом вместе с родителем), а событие в Google осталось:
-- триггер запоминает его, синк удаляет и стирает запись.
CREATE TABLE gcal_tombstones (
    calendar_id text NOT NULL,
    event_id    text NOT NULL,
    PRIMARY KEY (calendar_id, event_id)
);

CREATE FUNCTION items_gcal_tombstone() RETURNS trigger AS $$
BEGIN
    IF OLD.gcal_event_id IS NOT NULL THEN
        INSERT INTO gcal_tombstones (calendar_id, event_id)
        VALUES (OLD.gcal_calendar_id, OLD.gcal_event_id) ON CONFLICT DO NOTHING;
    END IF;
    RETURN OLD;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER items_gcal_tombstone BEFORE DELETE ON items
    FOR EACH ROW EXECUTE FUNCTION items_gcal_tombstone();

-- Задача из Google Tasks «Срочно» → входящее; id не даёт импортировать дважды.
ALTER TABLE inbox_messages ADD COLUMN gtask_id text UNIQUE;
