package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// ── kv: служебные значения ───────────────────────────────────────────────────

func (s *Store) KVGet(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRow(ctx, `SELECT value FROM kv WHERE key=$1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) KVSet(ctx context.Context, key, value string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO kv (key, value) VALUES ($1,$2)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, key, value)
	return err
}

// ── Синк с Google Calendar ───────────────────────────────────────────────────

// SyncedItem — задача вместе с её привязкой к событию Google.
type SyncedItem struct {
	domain.Item
	CalendarID *string
	EventID    *string
	Etag       *string
	Dirty      bool // изменена после последней выгрузки
}

func (s *Store) querySynced(ctx context.Context, where string, args ...any) ([]SyncedItem, error) {
	rows, err := s.db.Query(ctx, `SELECT `+itemCols+`, i.gcal_calendar_id, i.gcal_event_id, i.sync_etag,
		i.gcal_synced_at IS NULL OR i.updated_at > i.gcal_synced_at FROM items i WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SyncedItem
	for rows.Next() {
		var x SyncedItem
		var planned, occ *time.Time
		it := &x.Item
		if err := rows.Scan(&it.ID, &it.Kind, &it.Title, &it.Body, &it.Props, &it.SphereID, &it.ProjectID, &it.ParentID,
			&it.Status, &it.Important, &it.Urgent, &planned, &it.StartAt, &it.EndAt, &it.Deadline,
			&it.EstimateMin, &it.Weight, &it.PostponeCount, &it.RecurrenceID, &occ, &it.Source,
			&it.CreatedAt, &it.UpdatedAt, &it.DoneAt, &it.SpentMin, &it.Tags, &it.Progress,
			&x.CalendarID, &x.EventID, &x.Etag, &x.Dirty); err != nil {
			return nil, err
		}
		it.PlannedDate, it.OccurrenceDate = toDate(planned), toDate(occ)
		it.QuadrantN = it.Quadrant()
		out = append(out, x)
	}
	return out, rows.Err()
}

// ToPublish — что выгрузить в календарь calID: задачи и события со временем,
// новые или изменённые с прошлой выгрузки. Зеркала чужих календарей не трогаем.
func (s *Store) ToPublish(ctx context.Context, calID string) ([]SyncedItem, error) {
	return s.querySynced(ctx, `i.start_at IS NOT NULL AND i.kind IN ('task','event') AND i.status <> 'cancelled'
		AND (i.gcal_calendar_id IS NULL OR i.gcal_calendar_id = $1)
		AND (i.gcal_synced_at IS NULL OR i.updated_at > i.gcal_synced_at)`, calID)
}

// ToUnpublish — задачи, чьё событие больше не нужно: отменены, лишились времени или стали заметкой.
func (s *Store) ToUnpublish(ctx context.Context, calID string) ([]SyncedItem, error) {
	return s.querySynced(ctx, `i.gcal_calendar_id = $1 AND i.gcal_event_id IS NOT NULL
		AND (i.start_at IS NULL OR i.status = 'cancelled' OR i.kind NOT IN ('task','event'))`, calID)
}

// Linked — задачи, привязанные к событиям календаря и попадающие в окно
// (по времени или, для событий на весь день, по дате).
func (s *Store) Linked(ctx context.Context, calID string, from, to time.Time) ([]SyncedItem, error) {
	return s.querySynced(ctx, `i.gcal_calendar_id = $1 AND i.gcal_event_id IS NOT NULL AND (
		(i.start_at >= $2 AND i.start_at < $3) OR
		(i.start_at IS NULL AND i.planned_date >= $4::date AND i.planned_date < $5::date))`, calID, from, to,
		from.In(domain.MSK).Format(time.DateOnly), to.In(domain.MSK).Format(time.DateOnly))
}

func (s *Store) ByEvent(ctx context.Context, calID, eventID string) (*SyncedItem, error) {
	xs, err := s.querySynced(ctx, `i.gcal_calendar_id = $1 AND i.gcal_event_id = $2`, calID, eventID)
	if err != nil || len(xs) == 0 {
		return nil, err
	}
	return &xs[0], nil
}

func (s *Store) SyncedByID(ctx context.Context, id string) (*SyncedItem, error) {
	xs, err := s.querySynced(ctx, `i.id = $1`, id)
	if err != nil || len(xs) == 0 {
		return nil, err
	}
	return &xs[0], nil
}

// MarkSynced фиксирует, что задача и событие совпадают: updated_at не двигаем,
// а gcal_synced_at выравниваем по нему — иначе следующий синк выгрузил бы её снова.
func (s *Store) MarkSynced(ctx context.Context, id, calID, eventID, etag string) error {
	_, err := s.db.Exec(ctx, `UPDATE items SET gcal_calendar_id=$2, gcal_event_id=$3, sync_etag=$4,
		gcal_synced_at=updated_at WHERE id=$1`, id, calID, eventID, etag)
	return err
}

func (s *Store) Unlink(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE items SET gcal_calendar_id=NULL, gcal_event_id=NULL, sync_etag=NULL,
		gcal_synced_at=updated_at WHERE id=$1`, id)
	return err
}

type Tombstone struct{ CalendarID, EventID string }

func (s *Store) Tombstones(ctx context.Context) ([]Tombstone, error) {
	rows, err := s.db.Query(ctx, `SELECT calendar_id, event_id FROM gcal_tombstones`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tombstone, error) {
		var t Tombstone
		return t, r.Scan(&t.CalendarID, &t.EventID)
	})
}

func (s *Store) DropTombstone(ctx context.Context, t Tombstone) error {
	_, err := s.db.Exec(ctx, `DELETE FROM gcal_tombstones WHERE calendar_id=$1 AND event_id=$2`, t.CalendarID, t.EventID)
	return err
}

// AddInboxFromTask — задача из Google Tasks во входящие; повторный импорт молча пропускается.
func (s *Store) AddInboxFromTask(ctx context.Context, text, gtaskID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO inbox_messages (text, gtask_id) VALUES ($1,$2)
		ON CONFLICT (gtask_id) DO NOTHING`, text, gtaskID)
	return tag.RowsAffected() > 0, err
}
