package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Stats — сырьё для аналитики: Claude на разборе и (v3) графики в вебе.
type Stats struct {
	From domain.Date `json:"from"`
	To   domain.Date `json:"to"` // не включительно

	TimeBySphere []SphereTime   `json:"time_by_sphere"`
	Created      int            `json:"created"`
	Done         int            `json:"done"`
	Estimates    []EstimateFact `json:"estimates"` // закрытые за период с оценкой и фактом
	Postponed    []domain.Item  `json:"postponed"` // открытые разовые, перенесённые 2+ раза
	Stale        []domain.Item  `json:"stale"`     // открытые, созданы больше 14 дней назад
	DonePerDay   []DayCount     `json:"done_per_day"`

	// v3: ряды по каждому дню периода (без пропусков) и прошлый период для дельт.
	Days         []DayStat  `json:"days"`
	TotalMinutes int        `json:"total_minutes"`
	Prev         PrevPeriod `json:"prev"`
}

// DayStat — день периода: сделано/создано и выполнение плана. План дня = задачи с
// planned_date на этот день: сделанные, несделанные (только для прошедших дней)
// и перенесённые с этого дня на более поздний.
type DayStat struct {
	Date       domain.Date `json:"date"`
	Done       int         `json:"done"`
	Created    int         `json:"created"`
	Minutes    int         `json:"minutes"`
	PlanDone   int         `json:"plan_done"`
	PlanMissed int         `json:"plan_missed"`
	PlanMoved  int         `json:"plan_moved"`
}

type PrevPeriod struct {
	Done    int `json:"done"`
	Created int `json:"created"`
	Minutes int `json:"minutes"`
}

type SphereTime struct {
	Sphere  string `json:"sphere"`
	Minutes int    `json:"minutes"`
}

type EstimateFact struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Estimate int    `json:"estimate_min"`
	Spent    int    `json:"spent_min"`
}

type DayCount struct {
	Date  domain.Date `json:"date"`
	Count int         `json:"count"`
}

func (s *Store) Stats(ctx context.Context, from, to domain.Date) (Stats, error) {
	st := Stats{From: from, To: to}
	rows, err := s.db.Query(ctx, `SELECT COALESCE(sp.name, 'без сферы'), sum(t.minutes)
		FROM time_entries t JOIN items i ON i.id = t.item_id LEFT JOIN spheres sp ON sp.id = i.sphere_id
		WHERE COALESCE(t.started_at, t.created_at) >= $1 AND COALESCE(t.started_at, t.created_at) < $2
		GROUP BY 1 ORDER BY 2 DESC`, from.Time, to.Time)
	if err != nil {
		return st, err
	}
	if st.TimeBySphere, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SphereTime, error) {
		var x SphereTime
		return x, r.Scan(&x.Sphere, &x.Minutes)
	}); err != nil {
		return st, err
	}

	if err := s.db.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE created_at >= $1 AND created_at < $2),
			count(*) FILTER (WHERE done_at >= $1 AND done_at < $2)
		FROM items WHERE kind <> 'note'`, from.Time, to.Time).Scan(&st.Created, &st.Done); err != nil {
		return st, err
	}

	rows, err = s.db.Query(ctx, `SELECT i.id, i.title, i.estimate_min, sum(t.minutes)
		FROM items i JOIN time_entries t ON t.item_id = i.id
		WHERE i.done_at >= $1 AND i.done_at < $2 AND i.estimate_min IS NOT NULL
		GROUP BY i.id ORDER BY i.done_at`, from.Time, to.Time)
	if err != nil {
		return st, err
	}
	if st.Estimates, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (EstimateFact, error) {
		var x EstimateFact
		return x, r.Scan(&x.ID, &x.Title, &x.Estimate, &x.Spent)
	}); err != nil {
		return st, err
	}

	rows, err = s.db.Query(ctx, `SELECT (done_at AT TIME ZONE 'Europe/Moscow')::date, count(*)
		FROM items WHERE done_at >= $1 AND done_at < $2 GROUP BY 1 ORDER BY 1`, from.Time, to.Time)
	if err != nil {
		return st, err
	}
	if st.DonePerDay, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (DayCount, error) {
		var x DayCount
		var d domain.Date
		err := r.Scan(&d.Time, &x.Count)
		x.Date = *toDate(&d.Time)
		return x, err
	}); err != nil {
		return st, err
	}

	if st.Postponed, err = s.queryItems(ctx, `i.postpone_count >= 2 AND i.recurrence_id IS NULL
		AND i.status NOT IN ('done','cancelled') ORDER BY i.postpone_count DESC LIMIT 20`); err != nil {
		return st, err
	}
	if st.Stale, err = s.queryItems(ctx, `i.created_at < now() - interval '14 days' AND i.kind IN ('task','goal')
		AND i.status NOT IN ('done','cancelled','someday') ORDER BY i.created_at LIMIT 20`); err != nil {
		return st, err
	}
	if st.Days, err = s.dayStats(ctx, from, to); err != nil {
		return st, err
	}
	for _, d := range st.Days {
		st.TotalMinutes += d.Minutes
	}
	// Прошлый период той же длины — для дельт в плитках.
	n := int(to.Sub(from.Time).Hours()/24 + 0.5)
	pf := from.AddDays(-n)
	err = s.db.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM items WHERE kind <> 'note' AND done_at >= $1 AND done_at < $2),
			(SELECT count(*) FROM items WHERE kind <> 'note' AND created_at >= $1 AND created_at < $2),
			(SELECT COALESCE(sum(minutes), 0) FROM time_entries WHERE COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) < $2)`,
		pf.Time, from.Time).Scan(&st.Prev.Done, &st.Prev.Created, &st.Prev.Minutes)
	return st, err
}

func (s *Store) dayStats(ctx context.Context, from, to domain.Date) ([]DayStat, error) {
	byDay := map[string]*DayStat{}
	var out []DayStat
	for d := from; d.Before(to.Time); d = d.AddDays(1) {
		out = append(out, DayStat{Date: d})
	}
	for i := range out {
		byDay[out[i].Date.String()] = &out[i]
	}
	add := func(sql string, args []any, apply func(*DayStat, int, int)) error {
		rows, err := s.db.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d time.Time
			var a, b int
			if err := rows.Scan(&d, &a, &b); err != nil {
				return err
			}
			if ds := byDay[d.Format(time.DateOnly)]; ds != nil {
				apply(ds, a, b)
			}
		}
		return rows.Err()
	}
	ts := []any{from.Time, to.Time}
	ds := []any{from.String(), to.String(), domain.Today().String()}
	steps := []struct {
		sql   string
		args  []any
		apply func(*DayStat, int, int)
	}{
		{`SELECT (done_at AT TIME ZONE 'Europe/Moscow')::date, count(*), 0 FROM items
			WHERE kind <> 'note' AND done_at >= $1 AND done_at < $2 GROUP BY 1`, ts,
			func(d *DayStat, a, _ int) { d.Done = a }},
		{`SELECT (created_at AT TIME ZONE 'Europe/Moscow')::date, count(*), 0 FROM items
			WHERE kind <> 'note' AND created_at >= $1 AND created_at < $2 GROUP BY 1`, ts,
			func(d *DayStat, a, _ int) { d.Created = a }},
		{`SELECT (COALESCE(started_at, created_at) AT TIME ZONE 'Europe/Moscow')::date, sum(minutes)::int, 0 FROM time_entries
			WHERE COALESCE(started_at, created_at) >= $1 AND COALESCE(started_at, created_at) < $2 GROUP BY 1`, ts,
			func(d *DayStat, a, _ int) { d.Minutes = a }},
		// План дня: сделано / не сделано (только прошедшие дни).
		{`SELECT planned_date, count(*) FILTER (WHERE status = 'done'),
				count(*) FILTER (WHERE status NOT IN ('done','cancelled','someday') AND planned_date < $3::date)
			FROM items WHERE kind IN ('task','goal') AND planned_date >= $1::date AND planned_date < $2::date GROUP BY 1`, ds,
			func(d *DayStat, a, b int) { d.PlanDone, d.PlanMissed = a, b }},
		// Перенесённые с дня на более поздний (разовые; повторы не в счёт — как и в счётчике переносов).
		{`SELECT (e.old #>> '{}')::date, count(DISTINCT e.item_id)::int, 0 FROM item_events e JOIN items i ON i.id = e.item_id
			WHERE e.field = 'planned_date' AND e.old IS NOT NULL AND i.recurrence_id IS NULL
			  AND (e.old #>> '{}')::date >= $1::date AND (e.old #>> '{}')::date < $2::date
			  AND (e.new IS NULL OR (e.new #>> '{}')::date > (e.old #>> '{}')::date)
			GROUP BY 1`, ds[:2],
			func(d *DayStat, a, _ int) { d.PlanMoved = a }},
	}
	for _, st := range steps {
		if err := add(st.sql, st.args, st.apply); err != nil {
			return nil, err
		}
	}
	return out, nil
}
