-- Таймер: одна запущенная задача за раз (id всегда 1). Остановка пишет time_entry
-- с source = 'timer'; запуск другой задачи сначала останавливает текущую.
CREATE TABLE timers (
    id          int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    item_id     uuid NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    started_at  timestamptz NOT NULL DEFAULT now(),
    source      text NOT NULL DEFAULT 'web',
    reminded_at timestamptz -- бот уже напомнил, что таймер идёт слишком долго
);

-- Напоминания в Telegram: что уже отправлено. at — момент, к которому привязано
-- напоминание (start_at или deadline): перенесли задачу — напомнит заново.
CREATE TABLE reminders_sent (
    item_id uuid NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    kind    text NOT NULL CHECK (kind IN ('start', 'deadline_soon', 'deadline_missed')),
    at      timestamptz NOT NULL,
    sent_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (item_id, kind, at)
);

-- Здоровье по дням (Garmin или вручную): сон, стресс, Body Battery, шаги, пульс покоя.
CREATE TABLE health_days (
    date         date PRIMARY KEY,
    sleep_min    int CHECK (sleep_min IS NULL OR sleep_min BETWEEN 0 AND 1440),
    sleep_score  int CHECK (sleep_score IS NULL OR sleep_score BETWEEN 0 AND 100),
    stress_avg   int CHECK (stress_avg IS NULL OR stress_avg BETWEEN 0 AND 100),
    body_battery int CHECK (body_battery IS NULL OR body_battery BETWEEN 0 AND 100),
    steps        int CHECK (steps IS NULL OR steps >= 0),
    resting_hr   int CHECK (resting_hr IS NULL OR resting_hr BETWEEN 20 AND 250),
    source       text NOT NULL DEFAULT 'manual',
    updated_at   timestamptz NOT NULL DEFAULT now()
);
