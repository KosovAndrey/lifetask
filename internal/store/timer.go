package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
)

// Timer — запущенный таймер: над чем идёт работа и с какого момента.
type Timer struct {
	Item       domain.Item `json:"item"`
	StartedAt  time.Time   `json:"started_at"`
	ElapsedMin int         `json:"elapsed_min"`
	Source     string      `json:"source"`
}

// ActiveTimer — текущий таймер или nil.
func (s *Store) ActiveTimer(ctx context.Context) (*Timer, error) {
	var t Timer
	var itemID string
	err := s.db.QueryRow(ctx, `SELECT item_id, started_at, source FROM timers WHERE id = 1`).Scan(&itemID, &t.StartedAt, &t.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if t.Item, err = s.GetItem(ctx, itemID); err != nil {
		return nil, err
	}
	t.ElapsedMin = int(time.Since(t.StartedAt).Minutes())
	return &t, nil
}

// StartTimer запускает таймер на задаче (вызывать в транзакции). Если шёл таймер
// другой задачи — он останавливается и его время записывается (stopped).
// Задача из «к выполнению» переходит в «в работе».
func (s *Store) StartTimer(ctx context.Context, itemID, actor string) (t Timer, stopped *domain.TimeEntry, err error) {
	it, err := s.GetItem(ctx, itemID)
	if err != nil {
		return t, nil, err
	}
	if it.Status.Closed() {
		return t, nil, errors.New("задача уже закрыта — таймер не нужен")
	}
	cur, err := s.ActiveTimer(ctx)
	if err != nil {
		return t, nil, err
	}
	if cur != nil && cur.Item.ID == itemID {
		return *cur, nil, nil
	}
	if cur != nil {
		if stopped, err = s.StopTimer(ctx, ""); err != nil {
			return t, nil, err
		}
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO timers (id, item_id, source) VALUES (1, $1, $2)`, itemID, actor); err != nil {
		return t, stopped, err
	}
	if it.Status == domain.StatusTodo || it.Status == domain.StatusInbox {
		if it, err = s.UpdateItem(ctx, itemID, Patch{"status": json.RawMessage(`"doing"`)}, actor); err != nil {
			return t, stopped, err
		}
	}
	at, err := s.ActiveTimer(ctx)
	if err != nil || at == nil {
		return t, stopped, err
	}
	return *at, stopped, nil
}

// StopTimer останавливает таймер и записывает время (не меньше минуты).
// Нет таймера — (nil, nil).
func (s *Store) StopTimer(ctx context.Context, note string) (*domain.TimeEntry, error) {
	var itemID string
	var started time.Time
	err := s.db.QueryRow(ctx, `DELETE FROM timers WHERE id = 1 RETURNING item_id, started_at`).Scan(&itemID, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := max(1, int(math.Round(time.Since(started).Minutes())))
	e, err := s.LogTime(ctx, domain.TimeEntry{ItemID: itemID, StartedAt: &started, Minutes: m, Source: "timer", Note: note})
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// DiscardTimer — сбросить таймер, ничего не записывая (запустил по ошибке).
func (s *Store) DiscardTimer(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `DELETE FROM timers WHERE id = 1`)
	return err
}

// LongTimer — таймер, который идёт дольше after и о котором ещё не напоминали.
func (s *Store) LongTimer(ctx context.Context, after time.Duration) (*Timer, error) {
	var stale bool
	err := s.db.QueryRow(ctx, `SELECT started_at < now() - make_interval(secs => $1) AND reminded_at IS NULL
		FROM timers WHERE id = 1`, after.Seconds()).Scan(&stale)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !stale) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.ActiveTimer(ctx)
}

func (s *Store) MarkTimerReminded(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `UPDATE timers SET reminded_at = now() WHERE id = 1`)
	return err
}
