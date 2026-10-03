package bot

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/render"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

// Окна отправки брифов (MSK). Окно, а не точное время: если сервер лежал в 21:00,
// бриф уйдёт после подъёма, но не среди ночи.
const (
	eveningFrom, eveningTo = 21, 24
	morningFrom, morningTo = 7, 12
	weeklyFrom, weeklyTo   = 19, 21 // воскресенье, до вечернего брифа
)

// RunBriefs раз в минуту проверяет, не пора ли отправить бриф. Повторную
// отправку исключает уникальность (date, kind) в таблице briefs.
func (b *Bot) RunBriefs(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := b.briefTick(ctx); err != nil {
			slog.Error("brief", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Bot) briefTick(ctx context.Context) error {
	if b.owner == 0 {
		return nil
	}
	now := b.now().In(domain.MSK)
	today := domain.Today()
	switch h := now.Hour(); {
	case now.Weekday() == time.Sunday && h >= weeklyFrom && h < weeklyTo:
		return b.sendBrief(ctx, today, "weekly")
	case h >= eveningFrom && h < eveningTo:
		return b.sendBrief(ctx, today.AddDays(1), "evening")
	case h >= morningFrom && h < morningTo:
		return b.sendBrief(ctx, today, "morning")
	}
	return nil
}

// sendBrief: если на разборе Claude заготовил свой текст (plan brief), шлём его,
// иначе собираем стандартный из дня.
func (b *Bot) sendBrief(ctx context.Context, d domain.Date, kind string) error {
	body, sent, err := b.st.PendingBrief(ctx, d, kind)
	if err != nil || sent {
		return err
	}
	if body == "" {
		if body, err = b.composeBrief(ctx, d, kind); err != nil {
			return err
		}
	}
	var kb Keyboard
	if kind == "evening" {
		// Вечером — вопрос о дне: смайлик ставит настроение, ответ текстом идёт в дневник.
		day := d.AddDays(-1).String()
		body += "\n\n📔 Как прошёл день? Ответь на это сообщение — запишу в дневник."
		kb = Keyboard{{{"😞", "m:" + day + ":1"}, {"😕", "m:" + day + ":2"}, {"😐", "m:" + day + ":3"}, {"🙂", "m:" + day + ":4"}, {"😄", "m:" + day + ":5"}}}
	}
	msgID, err := b.m.Send(b.owner, body, kb)
	if err != nil {
		return err
	}
	if err := b.st.MarkBriefSent(ctx, d, kind, body); err != nil {
		return err
	}
	return b.st.SetBriefMessage(ctx, d, kind, msgID)
}

// journalDay — какой день описывает запись: до 4 утра пишут ещё про вчера.
func (b *Bot) journalDay() domain.Date {
	if b.now().In(domain.MSK).Hour() < 4 {
		return domain.Today().AddDays(-1)
	}
	return domain.Today()
}

func (b *Bot) composeBrief(ctx context.Context, d domain.Date, kind string) (string, error) {
	if kind == "weekly" {
		return b.composeWeekly(ctx, d)
	}
	day, err := b.st.Day(ctx, d)
	if err != nil {
		return "", err
	}
	o, err := b.opts(ctx)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	label := render.DayLabel(d.Time)
	if kind == "evening" {
		sb.WriteString("🌙 Завтра, " + label + "\n\n")
	} else {
		sb.WriteString("☀️ Сегодня, " + label + "\n\n")
	}
	sb.WriteString(render.Day(day, false, o))

	// Хвосты: утром — незакрытое с прошлых дней; вечером — ещё и то, что
	// было на сегодня и не сделано (оно станет просрочкой завтра).
	tails := day.Overdue
	if kind == "evening" {
		td, err := b.st.Day(ctx, domain.Today())
		if err != nil {
			return "", err
		}
		tails = td.Overdue
		for _, it := range td.Planned {
			if !it.Status.Closed() && it.Status != domain.StatusSomeday {
				tails = append(tails, it)
			}
		}
	}
	if len(tails) > 0 {
		fmt.Fprintf(&sb, "\n⚠️ Не закрыто: %d\n", len(tails))
		for i, it := range tails {
			if i == 5 {
				fmt.Fprintf(&sb, "    … и ещё %d\n", len(tails)-5)
				break
			}
			sb.WriteString("    " + render.Line(it, true, o) + "\n")
		}
	}
	if kind == "evening" {
		inbox, err := b.st.OpenInbox(ctx)
		if err != nil {
			return "", err
		}
		if len(inbox) > 0 {
			fmt.Fprintf(&sb, "\n📥 Во входящих %d — разберём?\n", len(inbox))
		}
	}
	return sb.String(), nil
}

// composeWeekly — итоги недели, которая заканчивается в воскресенье d, и загрузка следующей.
func (b *Bot) composeWeekly(ctx context.Context, sunday domain.Date) (string, error) {
	monday := sunday.AddDays(-6)
	st, err := b.st.Stats(ctx, monday, sunday.AddDays(1))
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 Неделя %s — %s\n\n", render.DayLabel(monday.Time), render.DayLabel(sunday.Time))
	fmt.Fprintf(&sb, "✅ Сделано: %d%s · создано: %d\n", st.Done, delta(st.Done, st.Prev.Done), st.Created)

	var planDone, planAll int
	for _, d := range st.Days {
		planDone += d.PlanDone
		planAll += d.PlanDone + d.PlanMissed + d.PlanMoved
	}
	if planAll > 0 {
		fmt.Fprintf(&sb, "🎯 План выполнен на %d%% (%d из %d)\n", planDone*100/planAll, planDone, planAll)
	}
	if st.TotalMinutes > 0 {
		fmt.Fprintf(&sb, "⏱ Учтено времени: %s%s\n", render.Mins(st.TotalMinutes), deltaMins(st.TotalMinutes, st.Prev.Minutes))
		for i, t := range st.TimeBySphere {
			if i == 4 {
				break
			}
			fmt.Fprintf(&sb, "    %s — %s\n", t.Sphere, render.Mins(t.Minutes))
		}
	}
	if acc, n := estimateAccuracy(st); n >= 3 {
		fmt.Fprintf(&sb, "📐 Оценки: факт ≈ %.1f× от оценки (по %d задачам)\n", acc, n)
	}
	if len(st.Postponed) > 0 {
		sb.WriteString("\n↻ Переносятся снова и снова:\n")
		for i, it := range st.Postponed {
			if i == 3 {
				break
			}
			fmt.Fprintf(&sb, "    %s — %d раз\n", it.Title, it.PostponeCount)
		}
	}
	if len(st.Stale) > 0 {
		fmt.Fprintf(&sb, "🕸 Висят дольше двух недель: %d\n", len(st.Stale))
	}

	sb.WriteString("\n📅 Следующая неделя:\n")
	for i := 1; i <= 7; i++ {
		d := sunday.AddDays(i)
		day, err := b.st.Day(ctx, d)
		if err != nil {
			return "", err
		}
		n := len(day.Scheduled) + len(day.Planned)
		if n == 0 {
			continue
		}
		fmt.Fprintf(&sb, "    %s — %d дел, %s\n", render.DayLabel(d.Time), n, render.Mins(day.BusyMin+day.PlanMin))
	}
	sb.WriteString("\nРазберём неделю вместе? Напиши Claude «недельный разбор».")
	return sb.String(), nil
}

func delta(cur, prev int) string {
	switch {
	case prev == 0 || cur == prev:
		return ""
	case cur > prev:
		return fmt.Sprintf(" (↑%d к прошлой)", cur-prev)
	}
	return fmt.Sprintf(" (↓%d к прошлой)", prev-cur)
}

func deltaMins(cur, prev int) string {
	switch {
	case prev == 0 || cur == prev:
		return ""
	case cur > prev:
		return " (↑" + render.Mins(cur-prev) + ")"
	}
	return " (↓" + render.Mins(prev-cur) + ")"
}

// estimateAccuracy — медиана «факт / оценка» по закрытым задачам с обоими значениями.
func estimateAccuracy(st store.Stats) (float64, int) {
	var ratios []float64
	for _, e := range st.Estimates {
		if e.Estimate > 0 {
			ratios = append(ratios, float64(e.Spent)/float64(e.Estimate))
		}
	}
	if len(ratios) == 0 {
		return 0, 0
	}
	slices.Sort(ratios)
	m := ratios[len(ratios)/2]
	if len(ratios)%2 == 0 {
		m = (ratios[len(ratios)/2-1] + m) / 2
	}
	return m, len(ratios)
}
