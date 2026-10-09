package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// HealthDay — показатели дня из Garmin (скрипт синхронизации) или введённые руками.
// Сон относится к дню, в который проснулся. nil — нет данных.
type HealthDay struct {
	Date        domain.Date `json:"date"`
	SleepMin    *int        `json:"sleep_min,omitempty"`
	SleepScore  *int        `json:"sleep_score,omitempty"`
	StressAvg   *int        `json:"stress_avg,omitempty"`
	BodyBattery *int        `json:"body_battery,omitempty"` // утренний заряд
	Steps       *int        `json:"steps,omitempty"`
	RestingHR   *int        `json:"resting_hr,omitempty"`
	Source      string      `json:"source"`
}

func (h HealthDay) empty() bool {
	return h.SleepMin == nil && h.SleepScore == nil && h.StressAvg == nil && h.BodyBattery == nil && h.Steps == nil && h.RestingHR == nil
}

const healthCols = `date, sleep_min, sleep_score, stress_avg, body_battery, steps, resting_hr, source`

func scanHealth(row pgx.Row) (HealthDay, error) {
	var h HealthDay
	var d time.Time
	err := row.Scan(&d, &h.SleepMin, &h.SleepScore, &h.StressAvg, &h.BodyBattery, &h.Steps, &h.RestingHR, &h.Source)
	if err == nil {
		h.Date = *toDate(&d)
	}
	return h, err
}

// Health — дни с данными в [from, to).
func (s *Store) Health(ctx context.Context, from, to domain.Date) ([]HealthDay, error) {
	rows, err := s.db.Query(ctx, `SELECT `+healthCols+` FROM health_days WHERE date >= $1 AND date < $2 ORDER BY date`,
		from.String(), to.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HealthDay, error) { return scanHealth(r) })
}

// PutHealth дописывает показатели дня: пустые поля не затирают уже записанные
// (часы присылают сон утром, а шаги — вечером).
func (s *Store) PutHealth(ctx context.Context, h HealthDay) (HealthDay, error) {
	if h.empty() {
		return h, errors.New("нет ни одного показателя")
	}
	if h.Source == "" {
		h.Source = "manual"
	}
	return scanHealth(s.db.QueryRow(ctx, `INSERT INTO health_days (date, sleep_min, sleep_score, stress_avg, body_battery, steps, resting_hr, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (date) DO UPDATE SET
			sleep_min    = COALESCE(EXCLUDED.sleep_min, health_days.sleep_min),
			sleep_score  = COALESCE(EXCLUDED.sleep_score, health_days.sleep_score),
			stress_avg   = COALESCE(EXCLUDED.stress_avg, health_days.stress_avg),
			body_battery = COALESCE(EXCLUDED.body_battery, health_days.body_battery),
			steps        = COALESCE(EXCLUDED.steps, health_days.steps),
			resting_hr   = COALESCE(EXCLUDED.resting_hr, health_days.resting_hr),
			source       = EXCLUDED.source,
			updated_at   = now()
		RETURNING `+healthCols,
		h.Date.String(), h.SleepMin, h.SleepScore, h.StressAvg, h.BodyBattery, h.Steps, h.RestingHR, h.Source))
}
