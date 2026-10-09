package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

func TestRemindersAndButtons(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	y, m, d := time.Now().In(domain.MSK).Date()
	noon := time.Date(y, m, d, 12, 0, 0, 0, domain.MSK)
	b.now = func() time.Time { return noon }

	start := noon.Add(10 * time.Minute)
	call, _ := st.CreateItem(ctx, domain.Item{Title: "Позвонить врачу", StartAt: &start}, "me")
	missed := noon.Add(-time.Hour)
	tax, _ := st.CreateItem(ctx, domain.Item{Title: "Налоги", Deadline: &missed}, "me")

	for range 2 {
		if err := b.remindTick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(tg.sent) != 2 {
		t.Fatalf("напоминаний: %d %+v", len(tg.sent), tg.sent)
	}
	var startMsg, dlMsg sent
	for _, s := range tg.sent {
		if strings.Contains(s.text, "Позвонить врачу") {
			startMsg = s
		} else {
			dlMsg = s
		}
	}
	if !strings.HasPrefix(startMsg.text, "⏰ Через 10м · 12:10 Позвонить врачу") {
		t.Fatalf("о начале: %q", startMsg.text)
	}
	if !strings.Contains(dlMsg.text, "🔥 Дедлайн прошёл") {
		t.Fatalf("о дедлайне: %q", dlMsg.text)
	}

	// ▶ запускает таймер, ✓ закрывает задачу, «на завтра» сдвигает дедлайн.
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "1", CallbackData: button(t, startMsg, "t:"), CallbackMsgID: startMsg.id})
	if tm, _ := st.ActiveTimer(ctx); tm == nil || tm.Item.ID != call.ID {
		t.Fatalf("таймер: %+v", tm)
	}
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "2", CallbackData: button(t, startMsg, "c:"), CallbackMsgID: startMsg.id})
	if it, _ := st.GetItem(ctx, call.ID); it.Status != domain.StatusDone {
		t.Fatalf("статус: %s", it.Status)
	}
	b.Handle(ctx, Incoming{ChatID: owner, CallbackID: "3", CallbackData: button(t, dlMsg, "r:"), CallbackMsgID: dlMsg.id})
	it, _ := st.GetItem(ctx, tax.ID)
	if it.PlannedDate.String() != domain.Today().AddDays(1).String() || clock(it.Deadline) != "23:59" {
		t.Fatalf("на завтра: %v %v", it.PlannedDate, it.Deadline)
	}

	// Тихие часы — молчит.
	late := noon.Add(2 * time.Hour)
	st.CreateItem(ctx, domain.Item{Title: "Ночное", StartAt: &late}, "me")
	b.now = func() time.Time { return late.Add(-5 * time.Minute) }
	if _, err := st.SaveSettings(ctx, []byte(`{"quiet_from": 13, "quiet_to": 15}`)); err != nil {
		t.Fatal(err)
	}
	n := len(tg.sent)
	b.remindTick(ctx)
	if len(tg.sent) != n {
		t.Fatalf("в тихие часы: %q", tg.last().text)
	}
}

func clock(t *time.Time) string { return t.In(domain.MSK).Format("15:04") }

func TestTimerCommands(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	today := domain.Today()
	it, _ := st.CreateItem(ctx, domain.Item{Title: "Уборка на кухне", PlannedDate: &today}, "me")

	b.Handle(ctx, Incoming{ChatID: owner, Text: "/timer"})
	if !strings.Contains(tg.last().text, "Над чем работаешь") || button(t, tg.last(), "t:") != "t:"+it.ID {
		t.Fatalf("/timer: %q", tg.last().text)
	}
	b.Handle(ctx, Incoming{ChatID: owner, Text: "/go уборку"})
	if !strings.HasPrefix(tg.last().text, "▶ Таймер: Уборка на кухне") {
		t.Fatalf("/go: %q", tg.last().text)
	}
	b.Handle(ctx, Incoming{ChatID: owner, Text: "/stop"})
	if !strings.HasPrefix(tg.last().text, "⏹ Записал 1м") {
		t.Fatalf("/stop: %q", tg.last().text)
	}
	if got, _ := st.GetItem(ctx, it.ID); got.SpentMin != 1 || got.Status != domain.StatusDoing {
		t.Fatalf("после таймера: %d %s", got.SpentMin, got.Status)
	}
	b.Handle(ctx, Incoming{ChatID: owner, Text: "/go несуществующее дело"})
	if !strings.Contains(tg.last().text, "не нашёл") {
		t.Fatalf("/go без совпадения: %q", tg.last().text)
	}
}

func TestMorningBriefLoad(t *testing.T) {
	ctx := context.Background()
	b, tg, _, st := setup(t, nil)
	today := domain.Today()
	st.SaveSettings(ctx, []byte(`{"capacity_min": 60}`))
	for _, title := range []string{"Первое", "Второе", "Третье"} {
		st.CreateItem(ctx, domain.Item{Title: title, PlannedDate: &today, EstimateMin: ptr(30)}, "me")
	}
	st.PutHealth(ctx, store.HealthDay{Date: today, SleepMin: ptr(300)})
	y, m, d := time.Now().In(domain.MSK).Date()
	b.now = func() time.Time { return time.Date(y, m, d, 7, 30, 0, 0, domain.MSK) }
	if err := b.briefTick(ctx); err != nil {
		t.Fatal(err)
	}
	text := tg.last().text
	if !strings.Contains(text, "⚖️ Дела 1ч30 при свободных 1ч — не влезает 1") || !strings.Contains(text, "😴 Сон 5ч — сил меньше обычного") {
		t.Fatalf("бриф: %q", text)
	}
}
