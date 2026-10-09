-- Monotonic versions detect edits independently of updated_at clock precision
-- and transaction timestamps. The trigger also covers direct SQL writers.
ALTER TABLE items ADD COLUMN version bigint NOT NULL DEFAULT 1;

CREATE FUNCTION increment_item_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.version := OLD.version + 1;
    RETURN NEW;
END;
$$;

-- Sync acknowledgements only change Google metadata and must not invalidate
-- an otherwise unchanged preview. UpdateItem writes these user fields even
-- for a tags-only patch, so that patch also advances the version.
CREATE TRIGGER items_version BEFORE UPDATE OF
    kind, title, body, props, sphere_id, project_id, parent_id,
    status, important, urgent, planned_date, start_at, end_at, deadline,
    estimate_min, weight, postpone_count, recurrence_id, occurrence_date,
    source, done_at ON items
    FOR EACH ROW EXECUTE FUNCTION increment_item_version();

-- NULL identifies legacy proposals without a recorded item snapshot. They
-- must be proposed again if they reference existing items.
ALTER TABLE change_plans ADD COLUMN item_versions jsonb;
