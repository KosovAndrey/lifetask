package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/parse"
	"gitlab.com/KosovAndrey/lifeplan/internal/render"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

// remindTick — напоминания о начале задач, дедлайнах и забытом таймере.
// Вызывается раз в минуту вместе с брифами; в тихие часы молчит.
func (b *Bot) remindTick(ctx context.Context) error {
	if b.owner == 0 {
		return nil
	}
	set, err := b.st.Settings(ctx)
	if err != nil {
		return err
	}
	now := b.now()
	if set.Quiet(now.In(domain.MSK).Hour()) {
		return nil
	}
	due, err := b.st.DueReminders(ctx, now, set)
	if err != nil {
		return err
	}
	for _, r := range due {
		ok, err := b.st.ClaimReminder(ctx, r)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		text, kb := reminderText(r, now)
		if _, err := b.m.Send(b.owner, text, kb); err != nil {
			slog.Error("tg: напоминание", "err", err)
		}
	}
	if set.TimerRemindMin > 0 {
		t, err := b.st.LongTimer(ctx, time.Duration(set.TimerRemindMin)*time.Minute)
		if err != nil {
			return err
		}
		if t != nil {
			if err := b.st.MarkTimerReminded(ctx); err != nil {
				return err
			}
			b.send(b.owner, fmt.Sprintf("⏱ Таймер на «%s» идёт уже %s — не забыл остановить?", t.Item.Title, render.Mins(t.ElapsedMin)),
				Keyboard{{{"⏹ Стоп и записать", "ts:"}, {"✗ Сбросить", "tx:"}}})
		}
	}
	return nil
}

func reminderText(r store.Reminder, now time.Time) (string, Keyboard) {
	it := r.Item
	done := Button{"✓ Готово", "c:" + it.ID}
	switch r.Kind {
	case store.RemindStart:
		left := int(math.Round(it.StartAt.Sub(now).Minutes()))
		when := it.StartAt.In(domain.MSK).Format("15:04")
		if it.EndAt != nil {
			when += "–" + it.EndAt.In(domain.MSK).Format("15:04")
		}
		text := fmt.Sprintf("⏰ Через %s · %s %s", render.Mins(max(1, left)), when, it.Title)
		if it.Kind == domain.KindEvent {
			return text, nil
		}
		return text, Keyboard{{{"▶ Таймер", "t:" + it.ID}, done}}
	case store.RemindDeadlineSoon:
		return fmt.Sprintf("⏳ Дедлайн %s: %s", deadlineLabel(*it.Deadline), it.Title),
			Keyboard{{done, {"▶ Таймер", "t:" + it.ID}}}
	default: // пропущенный дедлайн
		return fmt.Sprintf("🔥 Дедлайн прошёл (%s): %s\nСделать, сдвинуть или отменить?", deadlineLabel(*it.Deadline), it.Title),
			Keyboard{{done, {"📅 На завтра", "r:" + it.ID}, {"✗ Отменить", "k:" + it.ID}}}
	}
}

// deadlineLabel — «пт 10.10», а если время не конец дня — «пт 10.10 15:00».
func deadlineLabel(t time.Time) string {
	l := render.DayLabel(t)
	if hm := t.In(domain.MSK).Format("15:04"); hm != "23:59" {
		l += " " + hm
	}
	return l
}

// onItemCallback — кнопки под напоминаниями и таймером. ok=false — не наша кнопка.
func (b *Bot) onItemCallback(ctx context.Context, in Incoming, action, id string) (bool, error) {
	answer := func(text, edit string) error {
		_ = b.m.AnswerCallback(in.CallbackID, text)
		if edit != "" && in.CallbackMsgID != 0 {
			return b.m.Edit(b.owner, in.CallbackMsgID, edit, nil)
		}
		return nil
	}
	patch := func(p store.Patch) (domain.Item, error) {
		var it domain.Item
		err := b.st.InTx(ctx, func(tx *store.Store) error {
			var err error
			it, err = tx.UpdateItem(ctx, id, p, "bot")
			return err
		})
		return it, err
	}
	switch action {
	case "c": // готово
		it, err := patch(store.Patch{"status": json.RawMessage(`"done"`)})
		if err != nil {
			_ = answer("ошибка", "")
			return true, err
		}
		return true, answer("готово", "✅ "+it.Title)
	case "k": // отменить
		it, err := patch(store.Patch{"status": json.RawMessage(`"cancelled"`)})
		if err != nil {
			_ = answer("ошибка", "")
			return true, err
		}
		return true, answer("отменено", "✗ "+it.Title)
	case "r": // дедлайн и план — на завтра
		tomorrow := domain.Today().AddDays(1)
		dl := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 23, 59, 0, 0, domain.MSK)
		dlJSON, _ := json.Marshal(dl)
		it, err := patch(store.Patch{"deadline": dlJSON, "planned_date": json.RawMessage(`"` + tomorrow.String() + `"`)})
		if err != nil {
			_ = answer("ошибка", "")
			return true, err
		}
		return true, answer("на завтра", "📅 "+it.Title+" — дедлайн "+render.DayLabel(dl))
	case "t": // запустить таймер
		text, err := b.startTimer(ctx, id)
		if err != nil {
			return true, answer(err.Error(), "")
		}
		return true, answer("таймер запущен", text)
	case "ts":
		text, err := b.stopTimer(ctx)
		if err != nil {
			return true, err
		}
		return true, answer("остановлен", text)
	case "tx":
		if err := b.st.DiscardTimer(ctx); err != nil {
			return true, err
		}
		return true, answer("сброшен", "✗ Таймер сброшен, время не записано.")
	}
	return false, nil
}

func (b *Bot) startTimer(ctx context.Context, itemID string) (string, error) {
	var t store.Timer
	var stopped *domain.TimeEntry
	err := b.st.InTx(ctx, func(tx *store.Store) error {
		var err error
		t, stopped, err = tx.StartTimer(ctx, itemID, "bot")
		return err
	})
	if err != nil {
		return "", err
	}
	text := "▶ Таймер: " + t.Item.Title
	if stopped != nil {
		text = fmt.Sprintf("⏹ Прошлый таймер: записал %s.\n", render.Mins(stopped.Minutes)) + text
	}
	return text + "\n/stop — остановить и записать время.", nil
}

func (b *Bot) stopTimer(ctx context.Context) (string, error) {
	t, err := b.st.ActiveTimer(ctx)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "Таймер не запущен.", nil
	}
	e, err := b.st.StopTimer(ctx, "")
	if err != nil || e == nil {
		return "", err
	}
	return fmt.Sprintf("⏹ Записал %s на «%s».", render.Mins(e.Minutes), t.Item.Title), nil
}

// onTimerCommand: /timer — что идёт или кнопки запуска для дел дня;
// /go текст — запустить на задаче, похожей на текст; /stop — остановить.
func (b *Bot) onTimerCommand(ctx context.Context, cmd, arg string) error {
	switch cmd {
	case "/stop":
		text, err := b.stopTimer(ctx)
		if err != nil {
			return err
		}
		b.send(b.owner, text, nil)
		return nil
	case "/go":
		if strings.TrimSpace(arg) == "" {
			return b.onTimerCommand(ctx, "/timer", "")
		}
		cands, err := b.st.TimeCandidates(ctx, 50)
		if err != nil {
			return err
		}
		var pc []parse.Candidate
		for _, c := range cands {
			if !c.Status.Closed() {
				pc = append(pc, parse.Candidate{ID: c.ID, Title: c.Title})
			}
		}
		id := parse.BestMatch(arg, pc)
		if id == nil {
			return errors.New("не нашёл открытую задачу «" + arg + "» — /timer покажет дела дня")
		}
		text, err := b.startTimer(ctx, *id)
		if err != nil {
			return err
		}
		b.send(b.owner, text, nil)
		return nil
	}
	t, err := b.st.ActiveTimer(ctx)
	if err != nil {
		return err
	}
	if t != nil {
		b.send(b.owner, fmt.Sprintf("⏱ %s · %s", t.Item.Title, render.Mins(t.ElapsedMin)),
			Keyboard{{{"⏹ Стоп и записать", "ts:"}, {"✗ Сбросить", "tx:"}}})
		return nil
	}
	p, err := b.st.Plan(ctx, domain.Today())
	if err != nil {
		return err
	}
	var kb Keyboard
	for _, it := range append(p.Fits, p.Overflow...) {
		if len(kb) == 8 {
			break
		}
		kb = append(kb, []Button{{"▶ " + truncate(it.Title, 40), "t:" + it.ID}})
	}
	if len(kb) == 0 {
		b.send(b.owner, "Таймер не запущен, а открытых дел на сегодня нет. /go текст — запустить на любой задаче.", nil)
		return nil
	}
	b.send(b.owner, "Таймер не запущен. Над чем работаешь?", kb)
	return nil
}

// loadLine — загрузка дня одной строкой для брифа: «⚖️ План 7ч из 6ч свободных — не влезает 2».
func loadLine(p store.DayPlan) string {
	if p.LoadMin == 0 {
		return ""
	}
	s := fmt.Sprintf("⚖️ Дела %s при свободных %s", render.Mins(p.LoadMin), render.Mins(p.FreeMin))
	if n := len(p.Overflow); n > 0 {
		s += fmt.Sprintf(" — не влезает %d: перенеси лишнее в вебе («Сегодня»)", n)
	} else {
		s += " — влезает"
	}
	if p.Unestimated > 0 {
		s += fmt.Sprintf("\n    без оценки %d — посчитал по %s", p.Unestimated, render.Mins(store.DefaultEstimateMin))
	}
	return s
}

// healthLine — сон и заряд из Garmin, если есть: «😴 Сон 6ч10 · ⚡ 45».
func healthLine(h *store.HealthDay) string {
	if h == nil {
		return ""
	}
	var parts []string
	if h.SleepMin != nil {
		parts = append(parts, "😴 Сон "+render.Mins(*h.SleepMin))
	}
	if h.BodyBattery != nil {
		parts = append(parts, fmt.Sprintf("⚡ %d", *h.BodyBattery))
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, " · ")
	if (h.SleepMin != nil && *h.SleepMin < 360) || (h.BodyBattery != nil && *h.BodyBattery < 30) {
		s += " — сил меньше обычного, начни с главного"
	}
	return s
}
