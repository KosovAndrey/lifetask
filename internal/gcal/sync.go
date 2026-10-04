package gcal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

const (
	CalendarName = "LifeTask"
	TaskListName = "Срочно"
	Primary      = "primary"

	kvCalendar = "gcal_calendar_id"
	kvTaskList = "gtasks_list_id"
	actor      = "sync"
)

// Окна сверки. Свой календарь — шире: правки руками в Google бывают и на месяц вперёд.
// Основной — только ближайшее: он нужен для загрузки дня и брифов.
var (
	ownBack, ownAhead         = 7, 60
	primaryBack, primaryAhead = 1, 14
)

// Syncer сводит задачи с Google. Порядок за проход: сначала забрать правки из
// Google (чтобы не затереть их своей выгрузкой), потом выгрузить свои, потом
// импортировать «Срочно» из Tasks.
type Syncer struct {
	st  *store.Store
	api API
	now func() time.Time
}

func NewSyncer(st *store.Store, api API) *Syncer {
	return &Syncer{st: st, api: api, now: time.Now}
}

func (s *Syncer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			slog.Error("gcal sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Syncer) Once(ctx context.Context) error {
	calID, err := s.ensure(ctx, kvCalendar, func() (string, error) {
		id, err := s.api.FindCalendar(ctx, CalendarName)
		if errors.Is(err, ErrNotFound) {
			return s.api.CreateCalendar(ctx, CalendarName)
		}
		return id, err
	})
	if err != nil {
		return fmt.Errorf("календарь: %w", err)
	}
	var errs []error
	if err := s.pullOwn(ctx, calID); err != nil {
		errs = append(errs, fmt.Errorf("pull %s: %w", CalendarName, err))
	}
	if err := s.pullPrimary(ctx); err != nil {
		errs = append(errs, fmt.Errorf("pull primary: %w", err))
	}
	if err := s.push(ctx, calID); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Календарь удалили руками — пересоздадим на следующем проходе.
			_ = s.st.KVSet(ctx, kvCalendar, "")
		}
		errs = append(errs, fmt.Errorf("push: %w", err))
	}
	if err := s.importTasks(ctx); err != nil {
		errs = append(errs, fmt.Errorf("tasks: %w", err))
	}
	return errors.Join(errs...)
}

func (s *Syncer) ensure(ctx context.Context, key string, find func() (string, error)) (string, error) {
	if id, err := s.st.KVGet(ctx, key); err != nil || id != "" {
		return id, err
	}
	id, err := find()
	if err != nil {
		return "", err
	}
	return id, s.st.KVSet(ctx, key, id)
}

func (s *Syncer) window(back, ahead int) (time.Time, time.Time) {
	today := domain.Today()
	return today.AddDays(-back).Time, today.AddDays(ahead + 1).Time
}

// pullOwn — правки, сделанные руками в календаре LifeTask: перенос, переименование,
// удаление (→ задача отменяется) и новые события (→ новые задачи).
func (s *Syncer) pullOwn(ctx context.Context, calID string) error {
	from, to := s.window(ownBack, ownAhead)
	events, err := s.api.ListEvents(ctx, calID, from, to)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.ID] = true
		it, err := s.st.ByEvent(ctx, calID, e.ID)
		if err != nil {
			return err
		}
		if it == nil && e.LifeplanID != "" {
			if it, err = s.st.SyncedByID(ctx, e.LifeplanID); err != nil {
				return err
			}
			if it == nil {
				continue // задачу удалили у нас — событие уберёт выгрузка по надгробию
			}
		}
		if it == nil {
			if err := s.importEvent(ctx, calID, e, "event"); err != nil {
				return err
			}
			continue
		}
		if it.Etag != nil && *it.Etag == e.Etag {
			continue
		}
		if it.Dirty {
			continue // поменялось с обеих сторон — побеждает наша версия, её выгрузит push
		}
		if err := s.applyEvent(ctx, *it, calID, e); err != nil {
			return err
		}
	}
	// Событие пропало из окна, а задача не менялась — его удалили в Google.
	linked, err := s.st.Linked(ctx, calID, from, to)
	if err != nil {
		return err
	}
	for _, it := range linked {
		if seen[*it.EventID] || it.Dirty {
			continue
		}
		if !it.Status.Closed() {
			if _, err := s.st.UpdateItem(ctx, it.ID, store.Patch{"status": json.RawMessage(`"cancelled"`)}, actor); err != nil {
				return err
			}
		}
		if err := s.st.Unlink(ctx, it.ID); err != nil {
			return err
		}
	}
	return nil
}

// pullPrimary зеркалит основной календарь: встречи, добавленные руками, видны в
// дне и брифах. Зеркало только читается из Google — обратно не выгружается.
func (s *Syncer) pullPrimary(ctx context.Context) error {
	from, to := s.window(primaryBack, primaryAhead)
	events, err := s.api.ListEvents(ctx, Primary, from, to)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.ID] = true
		it, err := s.st.ByEvent(ctx, Primary, e.ID)
		if err != nil {
			return err
		}
		switch {
		case it == nil:
			err = s.importEvent(ctx, Primary, e, "event")
		case it.Etag == nil || *it.Etag != e.Etag:
			err = s.applyEvent(ctx, *it, Primary, e)
		}
		if err != nil {
			return err
		}
	}
	linked, err := s.st.Linked(ctx, Primary, from, to)
	if err != nil {
		return err
	}
	for _, it := range linked {
		if !seen[*it.EventID] {
			// Надгробие для primary игнорируется выгрузкой: чужое событие не удаляем.
			if err := s.st.DeleteItem(ctx, it.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func (s *Syncer) importEvent(ctx context.Context, calID string, e Event, kind string) error {
	it := domain.Item{Kind: domain.Kind(kind), Title: eventTitle(e), Source: "gcal"}
	if !setWhen(&it, e) {
		return nil
	}
	created, err := s.st.CreateItem(ctx, it, actor)
	if err != nil {
		return err
	}
	return s.st.MarkSynced(ctx, created.ID, calID, e.ID, e.Etag)
}

func (s *Syncer) applyEvent(ctx context.Context, it store.SyncedItem, calID string, e Event) error {
	var next domain.Item
	next.Title = eventTitle(e)
	if !setWhen(&next, e) {
		return nil
	}
	patch := store.Patch{}
	if next.Title != it.Title {
		patch["title"] = mustJSON(next.Title)
	}
	if !sameTime(next.StartAt, it.StartAt) {
		patch["start_at"] = mustJSON(next.StartAt)
	}
	if !sameTime(next.EndAt, it.EndAt) {
		patch["end_at"] = mustJSON(next.EndAt)
	}
	if e.Date != "" && (it.PlannedDate == nil || it.PlannedDate.String() != e.Date) {
		patch["planned_date"] = mustJSON(e.Date)
	}
	// Конец раньше старого начала даст ошибку валидации, если менять по одному —
	// поэтому патч целиком, одним UpdateItem.
	if len(patch) > 0 {
		if _, err := s.st.UpdateItem(ctx, it.ID, patch, actor); err != nil {
			return err
		}
	}
	return s.st.MarkSynced(ctx, it.ID, calID, e.ID, e.Etag)
}

// eventTitle снимает нашу пометку «✓ » у выполненных.
func eventTitle(e Event) string {
	t := strings.TrimPrefix(strings.TrimSpace(e.Summary), "✓ ")
	if t == "" {
		return "(без названия)"
	}
	return t
}

func setWhen(it *domain.Item, e Event) bool {
	switch {
	case e.Start != nil:
		it.StartAt, it.EndAt = e.Start, e.End
		return true
	case e.Date != "":
		d, err := domain.ParseDate(e.Date)
		if err != nil {
			return false
		}
		it.PlannedDate = &d
		return true
	}
	return false
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// push выгружает наши изменения: удалённые и снятые с календаря — убрать,
// новые и изменённые — создать или обновить.
func (s *Syncer) push(ctx context.Context, calID string) error {
	tombs, err := s.st.Tombstones(ctx)
	if err != nil {
		return err
	}
	for _, t := range tombs {
		if t.CalendarID == calID {
			if err := s.api.DeleteEvent(ctx, calID, t.EventID); err != nil {
				return err
			}
		}
		if err := s.st.DropTombstone(ctx, t); err != nil {
			return err
		}
	}

	gone, err := s.st.ToUnpublish(ctx, calID)
	if err != nil {
		return err
	}
	for _, it := range gone {
		if err := s.api.DeleteEvent(ctx, calID, *it.EventID); err != nil {
			return err
		}
		if err := s.st.Unlink(ctx, it.ID); err != nil {
			return err
		}
	}

	items, err := s.st.ToPublish(ctx, calID)
	if err != nil {
		return err
	}
	colors, err := s.sphereColors(ctx)
	if err != nil {
		return err
	}
	for _, it := range items {
		e := toEvent(it.Item, colors)
		var saved Event
		if it.EventID != nil {
			e.ID = *it.EventID
			saved, err = s.api.UpdateEvent(ctx, calID, e)
			if errors.Is(err, ErrNotFound) { // удалили руками, пока задача менялась — создаём заново
				e.ID = ""
				saved, err = s.api.InsertEvent(ctx, calID, e)
			}
		} else {
			saved, err = s.api.InsertEvent(ctx, calID, e)
		}
		if err != nil {
			return err
		}
		if err := s.st.MarkSynced(ctx, it.ID, calID, saved.ID, saved.Etag); err != nil {
			return err
		}
	}
	return nil
}

func toEvent(it domain.Item, colors map[int]string) Event {
	e := Event{Summary: it.Title, Start: it.StartAt, End: it.EndAt, LifeplanID: it.ID}
	if it.Status == domain.StatusDone {
		e.Summary = "✓ " + e.Summary
	}
	if it.SphereID != nil {
		e.ColorID = colors[*it.SphereID]
	}
	var desc []string
	for _, b := range it.Body {
		switch b["type"] {
		case "md", "callout":
			if t, ok := b["text"].(string); ok {
				desc = append(desc, t)
			}
		case "checklist":
			if items, ok := b["items"].([]any); ok {
				for _, x := range items {
					if m, ok := x.(map[string]any); ok {
						mark := "☐"
						if m["done"] == true {
							mark = "☑"
						}
						desc = append(desc, fmt.Sprintf("%s %v", mark, m["text"]))
					}
				}
			}
		case "link":
			desc = append(desc, fmt.Sprintf("%v", b["url"]))
		}
	}
	desc = append(desc, "— LifeTask")
	e.Description = strings.Join(desc, "\n")
	return e
}

// ── Цвета: ближайший из 11 цветов событий Google к цвету сферы ───────────────

var googleColors = map[string]string{
	"1": "#7986cb", "2": "#33b679", "3": "#8e24aa", "4": "#e67c73", "5": "#f6bf26", "6": "#f4511e",
	"7": "#039be5", "8": "#616161", "9": "#3f51b5", "10": "#0b8043", "11": "#d50000",
}

// defaultColors — цвет Google для стандартной палитры (миграция 008): подбор
// «ближайшего» склеил бы Карьеру с Учёбой (обе голубые). Действует, пока цвет
// сферы не меняли; поменяли — подбирается ближайший к новому.
var defaultColors = map[string]string{
	"#5451A4": "9", "#00C4C4": "7", "#006899": "1", "#993C23": "11",
	"#7C5700": "5", "#E79551": "6", "#73C076": "2",
}

// sphereColors: style.gcal_color (ручная настройка) → цвет по умолчанию → ближайший.
func (s *Syncer) sphereColors(ctx context.Context) (map[int]string, error) {
	ss, err := s.st.Spheres(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(ss))
	for _, sp := range ss {
		if c, ok := sp.Style["gcal_color"].(string); ok && googleColors[c] != "" {
			out[sp.ID] = c
		} else if c, ok := defaultColors[strings.ToUpper(sp.Color)]; ok {
			out[sp.ID] = c
		} else {
			out[sp.ID] = NearestColor(sp.Color)
		}
	}
	return out, nil
}

func NearestColor(hex string) string {
	r, g, b, ok := rgb(hex)
	if !ok {
		return ""
	}
	best, bestD := "", math.MaxFloat64
	for id, c := range googleColors {
		cr, cg, cb, _ := rgb(c)
		// Взвешенное расстояние («redmean») ближе к восприятию, чем евклидово.
		rm := (r + cr) / 2
		d := (2+rm/256)*(r-cr)*(r-cr) + 4*(g-cg)*(g-cg) + (2+(255-rm)/256)*(b-cb)*(b-cb)
		if d < bestD || d == bestD && id < best {
			best, bestD = id, d
		}
	}
	return best
}

func rgb(hex string) (float64, float64, float64, bool) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return float64(v >> 16), float64(v >> 8 & 0xff), float64(v & 0xff), true
}

// ── Google Tasks «Срочно» → входящие ─────────────────────────────────────────

func (s *Syncer) importTasks(ctx context.Context) error {
	listID, err := s.ensure(ctx, kvTaskList, func() (string, error) {
		id, err := s.api.FindTaskList(ctx, TaskListName)
		if errors.Is(err, ErrNotFound) {
			return s.api.CreateTaskList(ctx, TaskListName)
		}
		return id, err
	})
	if err != nil {
		return err
	}
	tasks, err := s.api.OpenTasks(ctx, listID)
	if errors.Is(err, ErrNotFound) {
		return s.st.KVSet(ctx, kvTaskList, "") // список удалили — найдём/создадим заново
	}
	if err != nil {
		return err
	}
	for _, t := range tasks {
		text := t.Title
		if t.Notes != "" {
			text += "\n" + t.Notes
		}
		if len(t.Due) >= 10 {
			text += "\nсрок: " + t.Due[:10]
		}
		if _, err := s.st.AddInboxFromTask(ctx, text, t.ID); err != nil {
			return err
		}
		if err := s.api.CompleteTask(ctx, listID, t.ID); err != nil {
			return err
		}
	}
	return nil
}
