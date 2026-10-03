package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/recur"
)

// RecurHorizonDays — на сколько дней вперёд создаются экземпляры повторов:
// достаточно для недельного плана и брифов, и не засоряет будущее.
const RecurHorizonDays = 14

type Recurrence struct {
	ID          string       `json:"id"`
	Rule        string       `json:"rule"`
	Human       string       `json:"human"`
	Start       domain.Date  `json:"start"`
	Until       *domain.Date `json:"until,omitempty"`
	StartTime   *string      `json:"start_time,omitempty"` // HH:MM
	DurationMin *int         `json:"duration_min,omitempty"`
	Template    domain.Item  `json:"template"`
	Stopped     bool         `json:"stopped"`
}

func (s *Store) CreateRecurrence(ctx context.Context, r Recurrence) (Recurrence, error) {
	rule, err := recur.Parse(r.Rule)
	if err != nil {
		return r, err
	}
	r.Human = rule.Human()
	t := &r.Template
	if t.Kind == "" {
		t.Kind = domain.KindTask
	}
	if t.Status == "" {
		t.Status = domain.StatusTodo
	}
	if t.Weight == 0 {
		t.Weight = 1
	}
	if err := t.Validate(); err != nil {
		return r, err
	}
	if t.PlannedDate != nil || t.StartAt != nil || t.Deadline != nil {
		return r, errors.New("у шаблона повтора не бывает дат: день задаёт правило, время — time/duration_min")
	}
	if r.StartTime != nil {
		if _, err := time.Parse("15:04", *r.StartTime); err != nil {
			return r, fmt.Errorf("time %q: ждём HH:MM", *r.StartTime)
		}
	}
	if r.DurationMin != nil && r.StartTime == nil {
		return r, errors.New("duration_min без time")
	}
	err = s.db.QueryRow(ctx, `INSERT INTO recurrences (rrule, dtstart, until, start_time, duration_min, template)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		r.Rule, r.Start.String(), dateArg(r.Until), r.StartTime, r.DurationMin, r.Template).Scan(&r.ID)
	return r, err
}

func (s *Store) recurrences(ctx context.Context, where string, args ...any) ([]Recurrence, error) {
	rows, err := s.db.Query(ctx, `SELECT id, rrule, dtstart, until, to_char(start_time, 'HH24:MI'), duration_min,
		template, stopped_at IS NOT NULL FROM recurrences WHERE `+where+` ORDER BY dtstart`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recurrence, error) {
		var r Recurrence
		var start time.Time
		var until *time.Time
		if err := row.Scan(&r.ID, &r.Rule, &start, &until, &r.StartTime, &r.DurationMin, &r.Template, &r.Stopped); err != nil {
			return r, err
		}
		r.Start, r.Until = *toDate(&start), toDate(until)
		if rule, err := recur.Parse(r.Rule); err == nil {
			r.Human = rule.Human()
		}
		return r, nil
	})
}

func (s *Store) Recurrences(ctx context.Context) ([]Recurrence, error) {
	return s.recurrences(ctx, `stopped_at IS NULL`)
}

// GenerateRecurrences создаёт экземпляры всех активных серий до horizon включительно.
// Уже созданные (или удалённые пользователем) даты не пересоздаются: generated_until
// двигается только вперёд.
func (s *Store) GenerateRecurrences(ctx context.Context, horizon domain.Date) (int, error) {
	rs, err := s.recurrences(ctx, `stopped_at IS NULL AND (until IS NULL OR until >= CURRENT_DATE)`)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, r := range rs {
		n, err := s.generate(ctx, r, horizon)
		if err != nil {
			return total, fmt.Errorf("повтор %s: %w", r.ID, err)
		}
		total += n
	}
	return total, nil
}

// GenerateOne — для только что созданной серии (в той же транзакции).
func (s *Store) GenerateOne(ctx context.Context, id string, horizon domain.Date) ([]domain.Item, error) {
	rs, err := s.recurrences(ctx, `id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, ErrNotFound
	}
	if _, err := s.generate(ctx, rs[0], horizon); err != nil {
		return nil, err
	}
	return s.queryItems(ctx, `i.recurrence_id = $1 ORDER BY i.occurrence_date`, id)
}

func (s *Store) generate(ctx context.Context, r Recurrence, horizon domain.Date) (int, error) {
	rule, err := recur.Parse(r.Rule)
	if err != nil {
		return 0, err
	}
	var genUntil *time.Time
	if err := s.db.QueryRow(ctx, `SELECT generated_until FROM recurrences WHERE id=$1 FOR UPDATE`, r.ID).Scan(&genUntil); err != nil {
		return 0, err
	}
	from := r.Start
	if g := toDate(genUntil); g != nil && !g.Before(from.Time) {
		from = g.AddDays(1)
	}
	to := horizon
	if r.Until != nil && r.Until.Before(to.Time) {
		to = *r.Until
	}
	n := 0
	for _, d := range rule.Between(r.Start, from, to) {
		it := r.Template
		it.ID = ""
		it.RecurrenceID = &r.ID
		occ := d
		it.OccurrenceDate = &occ
		if r.StartTime != nil {
			hm, _ := time.Parse("15:04", *r.StartTime)
			start := time.Date(d.Year(), d.Month(), d.Day(), hm.Hour(), hm.Minute(), 0, 0, domain.MSK)
			it.StartAt = &start
			if r.DurationMin != nil {
				end := start.Add(time.Duration(*r.DurationMin) * time.Minute)
				it.EndAt = &end
			}
		} else {
			it.PlannedDate = &occ
		}
		if _, err := s.CreateItem(ctx, it, "recur"); err != nil {
			return n, err
		}
		n++
	}
	if !to.Before(from.Time) {
		if _, err := s.db.Exec(ctx, `UPDATE recurrences SET generated_until=$2 WHERE id=$1`, r.ID, to.String()); err != nil {
			return n, err
		}
	}
	return n, nil
}

// StopRecurrence завершает серию с даты from: будущие несделанные экземпляры
// удаляются, сделанные и начатые остаются для истории и аналитики.
func (s *Store) StopRecurrence(ctx context.Context, id string, from domain.Date) (Recurrence, int, error) {
	rs, err := s.recurrences(ctx, `id = $1`, id)
	if err != nil {
		return Recurrence{}, 0, err
	}
	if len(rs) == 0 {
		return Recurrence{}, 0, ErrNotFound
	}
	if _, err := s.db.Exec(ctx, `UPDATE recurrences SET stopped_at=now(), until=$2 WHERE id=$1`,
		id, from.AddDays(-1).String()); err != nil {
		return rs[0], 0, err
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM items WHERE recurrence_id=$1 AND occurrence_date >= $2
		AND status IN ('todo','inbox','someday')`, id, from.String())
	if err != nil {
		return rs[0], 0, err
	}
	return rs[0], int(tag.RowsAffected()), nil
}

// HumanDates — «сб 10.10, сб 17.10» для превью.
func HumanDates(items []domain.Item, limit int) string {
	var out []string
	for i, it := range items {
		if i == limit {
			out = append(out, "…")
			break
		}
		if it.OccurrenceDate != nil {
			out = append(out, fmt.Sprintf("%s %02d.%02d", weekdayShort[it.OccurrenceDate.Weekday()],
				it.OccurrenceDate.Day(), it.OccurrenceDate.Month()))
		}
	}
	return strings.Join(out, ", ")
}

var weekdayShort = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
