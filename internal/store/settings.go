package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Settings — настройки пользователя (kv «settings», JSON). Отсутствующее поле
// берёт значение по умолчанию, поэтому новые настройки не требуют миграций.
type Settings struct {
	// CapacityMin — сколько времени в дне на дела, вместе со встречами.
	CapacityMin int `json:"capacity_min"`
	// RemindBeforeMin — за сколько минут напоминать о задаче со временем; 0 — не напоминать.
	RemindBeforeMin int `json:"remind_before_min"`
	// RemindDeadlines — предупреждать о близком и пропущенном дедлайне.
	RemindDeadlines bool `json:"remind_deadlines"`
	// Тихие часы (MSK): с QuietFrom до QuietTo бот не напоминает.
	QuietFrom int `json:"quiet_from"`
	QuietTo   int `json:"quiet_to"`
	// TimerRemindMin — напомнить, если таймер идёт дольше; 0 — не напоминать.
	TimerRemindMin int `json:"timer_remind_min"`
}

const kvSettings = "settings"

func DefaultSettings() Settings {
	return Settings{CapacityMin: 480, RemindBeforeMin: 15, RemindDeadlines: true, QuietFrom: 23, QuietTo: 8, TimerRemindMin: 180}
}

func (s *Store) Settings(ctx context.Context) (Settings, error) {
	st := DefaultSettings()
	raw, err := s.KVGet(ctx, kvSettings)
	if err != nil || raw == "" {
		return st, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return DefaultSettings(), fmt.Errorf("настройки в kv испорчены: %w", err)
	}
	return st, nil
}

// SaveSettings накладывает patch (часть полей Settings в JSON) на текущие настройки.
func (s *Store) SaveSettings(ctx context.Context, patch json.RawMessage) (Settings, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return st, err
	}
	next := st
	dec := json.NewDecoder(bytes.NewReader(patch))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return st, fmt.Errorf("настройки: %w", err)
	}
	if err := next.validate(); err != nil {
		return st, err
	}
	b, _ := json.Marshal(next)
	return next, s.KVSet(ctx, kvSettings, string(b))
}

func (st Settings) validate() error {
	switch {
	case st.CapacityMin < 30 || st.CapacityMin > 1440:
		return errors.New("время на дела в день — от 30 минут до 24 часов")
	case st.RemindBeforeMin < 0 || st.RemindBeforeMin > 240:
		return errors.New("напоминание — от 0 до 240 минут")
	case st.QuietFrom < 0 || st.QuietFrom > 23 || st.QuietTo < 0 || st.QuietTo > 23:
		return errors.New("тихие часы — от 0 до 23")
	case st.TimerRemindMin < 0 || st.TimerRemindMin > 1440:
		return errors.New("напоминание о таймере — от 0 до 1440 минут")
	}
	return nil
}

// Quiet — попадает ли час (MSK) в тихие часы; окно может переходить через полночь.
func (st Settings) Quiet(hour int) bool {
	if st.QuietFrom == st.QuietTo {
		return false
	}
	if st.QuietFrom < st.QuietTo {
		return hour >= st.QuietFrom && hour < st.QuietTo
	}
	return hour >= st.QuietFrom || hour < st.QuietTo
}
