// Package render — компактный текст задач и дней: общий для CLI (с id, для Claude)
// и Telegram (без id, для человека).
package render

import (
	"fmt"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

type Opts struct {
	IDs     bool           // дописывать #id — нужно Claude для планов
	Spheres map[int]string // id → подпись сферы
}

var Weekdays = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}

func Mins(m int) string {
	switch {
	case m >= 60 && m%60 == 0:
		return fmt.Sprintf("%dч", m/60)
	case m >= 60:
		return fmt.Sprintf("%dч%02d", m/60, m%60)
	}
	return fmt.Sprintf("%dм", m)
}

func DayLabel(t time.Time) string {
	t = t.In(domain.MSK)
	return fmt.Sprintf("%s %02d.%02d", Weekdays[t.Weekday()], t.Day(), t.Month())
}

var statusMark = map[domain.Status]string{
	domain.StatusDone: "✓", domain.StatusCancelled: "✗", domain.StatusDoing: "▶",
	domain.StatusWaiting: "⏸", domain.StatusSomeday: "…", domain.StatusInbox: "📥",
}

// Line — одна задача. withDate добавляет день (для дедлайнов, просрочки, списков).
func Line(it domain.Item, withDate bool, o Opts) string {
	var b strings.Builder
	mark := statusMark[it.Status]
	if mark == "" {
		mark = "•"
	}
	fmt.Fprintf(&b, "%s Q%d ", mark, it.Quadrant())
	switch {
	case it.StartAt != nil:
		if withDate {
			b.WriteString(DayLabel(*it.StartAt) + " ")
		}
		b.WriteString(it.StartAt.In(domain.MSK).Format("15:04"))
		if it.EndAt != nil {
			b.WriteString("–" + it.EndAt.In(domain.MSK).Format("15:04"))
		}
		b.WriteString(" ")
	case withDate && it.PlannedDate != nil:
		b.WriteString(DayLabel(it.PlannedDate.Time) + " ")
	}
	if it.Kind != domain.KindTask {
		b.WriteString("[" + string(it.Kind) + "] ")
	}
	b.WriteString(it.Title)
	if it.RecurrenceID != nil {
		b.WriteString(" 🔁")
	}

	var meta []string
	if it.SphereID != nil && o.Spheres != nil {
		meta = append(meta, o.Spheres[*it.SphereID])
	}
	if it.EstimateMin != nil || it.SpentMin > 0 {
		est := "?"
		if it.EstimateMin != nil {
			est = Mins(*it.EstimateMin)
		}
		if it.SpentMin > 0 {
			meta = append(meta, fmt.Sprintf("~%s/факт %s", est, Mins(it.SpentMin)))
		} else {
			meta = append(meta, "~"+est)
		}
	}
	if it.Deadline != nil {
		meta = append(meta, "дедлайн "+DayLabel(*it.Deadline))
	}
	if it.PostponeCount > 0 {
		meta = append(meta, fmt.Sprintf("переносов %d", it.PostponeCount))
	}
	meta = append(meta, it.Tags...)
	if len(meta) > 0 {
		b.WriteString("  (" + strings.Join(meta, ", ") + ")")
	}
	if o.IDs {
		b.WriteString("  #" + it.ID)
	}
	return b.String()
}

// Day — день целиком: шапка с загрузкой, затем секции.
func Day(d store.Day, withOverdue bool, o Opts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "── %s %s · встречи %s · план %s\n", Weekdays[d.Date.Weekday()], d.Date, Mins(d.BusyMin), Mins(d.PlanMin))
	section := func(name string, items []domain.Item, withDate bool) {
		if len(items) == 0 {
			return
		}
		b.WriteString("  " + name + ":\n")
		for _, it := range items {
			b.WriteString("    " + Line(it, withDate, o) + "\n")
		}
	}
	section("по времени", d.Scheduled, false)
	section("задачи", d.Planned, false)
	section("дедлайны", d.Deadlines, true)
	if withOverdue {
		section("просрочено", d.Overdue, true)
	}
	if len(d.Scheduled)+len(d.Planned)+len(d.Deadlines) == 0 {
		b.WriteString("  пусто\n")
	}
	return b.String()
}
