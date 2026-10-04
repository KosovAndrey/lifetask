-- Вложения, присланные боту до разбора заметки: при принятии переезжают в body задачи.
ALTER TABLE inbox_messages ADD COLUMN files jsonb NOT NULL DEFAULT '[]';
-- Поиск блока файла по file_id (выдача /api/files/{id}).
CREATE INDEX items_body_gin ON items USING gin (body jsonb_path_ops);
