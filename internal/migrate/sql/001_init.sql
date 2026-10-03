-- Ядро lifeplan. Логика — в типизированных колонках, оформление — в body (блоки) и props.

CREATE TABLE spheres (
    id       serial PRIMARY KEY,
    slug     text NOT NULL UNIQUE,
    name     text NOT NULL,
    color    text NOT NULL,
    icon     text NOT NULL DEFAULT '',
    style    jsonb NOT NULL DEFAULT '{}',
    sort     int NOT NULL DEFAULT 0,
    archived bool NOT NULL DEFAULT false
);

INSERT INTO spheres (slug, name, color, icon, sort) VALUES
    ('work',    'Работа',   '#3B82F6', '💼', 10),
    ('career',  'Карьера',  '#8B5CF6', '🎯', 20),
    ('study',   'Учёба',    '#06B6D4', '📚', 30),
    ('product', 'Продукты', '#F59E0B', '🚀', 40),
    ('home',    'Быт',      '#10B981', '🏠', 50),
    ('leisure', 'Досуг',    '#EC4899', '🎮', 60),
    ('health',  'Здоровье', '#EF4444', '💪', 70),
    ('finance', 'Финансы',  '#84CC16', '💰', 80);

-- Проекты и группы проектов: дерево любой глубины через parent_id.
CREATE TABLE projects (
    id        serial PRIMARY KEY,
    parent_id int REFERENCES projects(id) ON DELETE SET NULL,
    sphere_id int REFERENCES spheres(id),
    name      text NOT NULL,
    color     text NOT NULL DEFAULT '',
    archived  bool NOT NULL DEFAULT false
);

CREATE TABLE tags (
    id    serial PRIMARY KEY,
    name  text NOT NULL UNIQUE,
    kind  text NOT NULL DEFAULT 'label' CHECK (kind IN ('context', 'label')),
    color text NOT NULL DEFAULT ''
);

CREATE TABLE recurrences (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_item_id uuid,
    rrule            text NOT NULL,
    until            date,
    generated_until  date,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             text NOT NULL DEFAULT 'task' CHECK (kind IN ('task', 'goal', 'event', 'note')),
    title            text NOT NULL CHECK (title <> ''),
    body             jsonb NOT NULL DEFAULT '[]',
    props            jsonb NOT NULL DEFAULT '{}',

    sphere_id        int REFERENCES spheres(id),
    project_id       int REFERENCES projects(id) ON DELETE SET NULL,
    parent_id        uuid REFERENCES items(id) ON DELETE CASCADE,

    status           text NOT NULL DEFAULT 'todo'
                     CHECK (status IN ('inbox', 'todo', 'doing', 'waiting', 'done', 'cancelled', 'someday')),
    important        bool NOT NULL DEFAULT false,
    urgent           bool NOT NULL DEFAULT false,

    planned_date     date,
    start_at         timestamptz,
    end_at           timestamptz,
    deadline         timestamptz,
    estimate_min     int CHECK (estimate_min IS NULL OR estimate_min >= 0),
    weight           numeric NOT NULL DEFAULT 1 CHECK (weight >= 0),
    postpone_count   int NOT NULL DEFAULT 0,

    recurrence_id    uuid REFERENCES recurrences(id) ON DELETE SET NULL,
    occurrence_date  date,

    source           text NOT NULL DEFAULT 'web' CHECK (source IN ('tg', 'web', 'claude', 'gcal', 'gtasks')),
    gcal_calendar_id text,
    gcal_event_id    text,
    gtask_id         text,
    sync_etag        text,

    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    done_at          timestamptz,

    CHECK (end_at IS NULL OR start_at IS NOT NULL),
    CHECK (end_at IS NULL OR end_at >= start_at)
);

ALTER TABLE recurrences
    ADD CONSTRAINT recurrences_template_fk FOREIGN KEY (template_item_id) REFERENCES items(id) ON DELETE CASCADE;

CREATE INDEX items_planned_idx  ON items (planned_date) WHERE status NOT IN ('done', 'cancelled');
CREATE INDEX items_start_idx    ON items (start_at);
CREATE INDEX items_deadline_idx ON items (deadline) WHERE deadline IS NOT NULL;
CREATE INDEX items_parent_idx   ON items (parent_id);
CREATE INDEX items_status_idx   ON items (status);
CREATE UNIQUE INDEX items_occurrence_uq ON items (recurrence_id, occurrence_date) WHERE recurrence_id IS NOT NULL;
CREATE UNIQUE INDEX items_gcal_uq ON items (gcal_calendar_id, gcal_event_id) WHERE gcal_event_id IS NOT NULL;

CREATE TABLE item_tags (
    item_id uuid REFERENCES items(id) ON DELETE CASCADE,
    tag_id  int REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (item_id, tag_id)
);

-- Связи помимо parent_id.
CREATE TABLE relations (
    from_id    uuid REFERENCES items(id) ON DELETE CASCADE,
    to_id      uuid REFERENCES items(id) ON DELETE CASCADE,
    type       text NOT NULL CHECK (type IN ('blocks', 'related', 'follows', 'part_of', 'prepares')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (from_id, to_id, type),
    CHECK (from_id <> to_id)
);
CREATE INDEX relations_to_idx ON relations (to_id);

CREATE TABLE time_entries (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id    uuid REFERENCES items(id) ON DELETE CASCADE,
    started_at timestamptz,
    minutes    int NOT NULL CHECK (minutes > 0),
    source     text NOT NULL DEFAULT 'web' CHECK (source IN ('garmin_manual', 'tg', 'web', 'timer', 'claude')),
    note       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX time_entries_item_idx ON time_entries (item_id);

-- Журнал изменений: на нём строится аналитика (переносы, время в статусах, долгожители).
CREATE TABLE item_events (
    id      bigserial PRIMARY KEY,
    item_id uuid NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    at      timestamptz NOT NULL DEFAULT now(),
    actor   text NOT NULL,
    field   text NOT NULL,
    old     jsonb,
    new     jsonb
);
CREATE INDEX item_events_item_idx ON item_events (item_id, at);

CREATE TABLE journal_days (
    date       date PRIMARY KEY,
    body       jsonb NOT NULL DEFAULT '[]',
    mood       int CHECK (mood BETWEEN 1 AND 5),
    summary    text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE inbox_messages (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tg_message_id bigint,
    text          text NOT NULL DEFAULT '',
    voice_file_id text,
    transcript    text,
    parsed        jsonb,
    status        text NOT NULL DEFAULT 'new'
                  CHECK (status IN ('new', 'proposed', 'accepted', 'deferred', 'rejected')),
    item_id       uuid REFERENCES items(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inbox_open_idx ON inbox_messages (created_at) WHERE status IN ('new', 'proposed', 'deferred');

-- Изменения от ИИ: сначала план, потом подтверждение и атомарное применение.
CREATE TABLE change_plans (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    author     text NOT NULL CHECK (author IN ('claude', 'bot', 'me')),
    summary    text NOT NULL DEFAULT '',
    ops        jsonb NOT NULL,
    status     text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'applied', 'rejected')),
    result     jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    applied_at timestamptz
);

CREATE TABLE briefs (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    date    date NOT NULL,
    kind    text NOT NULL CHECK (kind IN ('evening', 'morning', 'weekly')),
    body    text NOT NULL,
    sent_at timestamptz,
    UNIQUE (date, kind)
);
