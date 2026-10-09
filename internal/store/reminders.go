package store

import (
	"context"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Виды напоминаний.
const (
	RemindStart          = "start"           // скоро начало задачи или встречи
	RemindDeadlineSoon   = "deadline_soon"   // дедлайн в ближайшие DeadlineSoonWindow
	RemindDeadlineMissed = "deadline_missed" // дедлайн прошёл, задача открыта
)

// DeadlineSoonWindow — за сколько предупреждать о дедлайне. Дедлайн из веба ставится
// на 23:59, и 36 часов дают предупреждение днём накануне, а не в последнюю ночь.
const DeadlineSoonWindow = 36 * time.Hour

type Reminder struct {
	Kind string      `json:"kind"`
	At   time.Time   `json:"at"` // start_at или deadline, к которому привязано
	Item domain.Item `json:"item"`
}

// DueReminders — что пора напомнить и ещё не напоминалось. События основного
// календаря Google пропускаем: о них напоминает сам Google.
func (s *Store) DueReminders(ctx context.Context, now time.Time, set Settings) ([]Reminder, error) {
	var out []Reminder
	add := func(kind string, at func(domain.Item) time.Time, where string, args ...any) error {
		items, err := s.queryItems(ctx, where, args...)
		if err != nil {
			return err
		}
		for _, it := range items {
			out = append(out, Reminder{Kind: kind, At: at(it), Item: it})
		}
		return nil
	}
	const open = `i.status NOT IN ('done','cancelled','someday') AND i.kind IN ('task','event')
		AND i.gcal_calendar_id IS DISTINCT FROM 'primary'`
	notSent := func(kind, col string) string {
		return ` AND NOT EXISTS (SELECT 1 FROM reminders_sent r WHERE r.item_id = i.id AND r.kind = '` + kind + `' AND r.at = i.` + col + `)`
	}
	if set.RemindBeforeMin > 0 {
		if err := add(RemindStart, func(it domain.Item) time.Time { return *it.StartAt },
			open+` AND i.start_at > $1 AND i.start_at <= $2`+notSent(RemindStart, "start_at")+` ORDER BY i.start_at`,
			now, now.Add(time.Duration(set.RemindBeforeMin)*time.Minute)); err != nil {
			return nil, err
		}
	}
	if set.RemindDeadlines {
		dl := func(it domain.Item) time.Time { return *it.Deadline }
		if err := add(RemindDeadlineSoon, dl,
			open+` AND i.deadline > $1 AND i.deadline <= $2`+notSent(RemindDeadlineSoon, "deadline")+` ORDER BY i.deadline`,
			now, now.Add(DeadlineSoonWindow)); err != nil {
			return nil, err
		}
		// Пропущенные — только за последнюю неделю: старые долги и так видны в разборе.
		if err := add(RemindDeadlineMissed, dl,
			open+` AND i.deadline <= $1 AND i.deadline > $2`+notSent(RemindDeadlineMissed, "deadline")+` ORDER BY i.deadline`,
			now, now.Add(-7*24*time.Hour)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ClaimReminder отмечает напоминание отправленным; false — его уже отправили.
// Отмечаем до отправки: лучше потерять одно напоминание, чем слать его каждую минуту.
func (s *Store) ClaimReminder(ctx context.Context, r Reminder) (bool, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO reminders_sent (item_id, kind, at) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
		r.Item.ID, r.Kind, r.At)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
