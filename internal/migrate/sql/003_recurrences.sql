-- Повторы: шаблон задачи хранится в самой серии (jsonb), а не отдельной задачей —
-- иначе шаблон пришлось бы прятать из всех выборок. Экземпляры — обычные items
-- с recurrence_id + occurrence_date (уникальная пара), генерируются на 14 дней вперёд.
ALTER TABLE recurrences
    DROP COLUMN template_item_id,
    ADD COLUMN template     jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN dtstart      date NOT NULL DEFAULT CURRENT_DATE,
    ADD COLUMN start_time   time,
    ADD COLUMN duration_min int CHECK (duration_min IS NULL OR duration_min > 0),
    ADD COLUMN stopped_at   timestamptz;
