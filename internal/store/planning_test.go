package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

func TestPlanOverflowAndReschedule(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	today := domain.Today()
	if _, err := st.SaveSettings(ctx, json.RawMessage(`{"capacity_min": 180}`)); err != nil {
		t.Fatal(err)
	}
	mk := func(it domain.Item) domain.Item {
		it.PlannedDate = &today
		out, err := st.CreateItem(ctx, it, "me")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Встреча на час: свободно 2ч.
	start := time.Date(today.Year(), today.Month(), today.Day(), 10, 0, 0, 0, domain.MSK)
	meet := mk(domain.Item{Title: "Созвон", Kind: domain.KindEvent, StartAt: &start, EndAt: ptr(start.Add(time.Hour))})
	q4 := mk(domain.Item{Title: "Неважное", EstimateMin: ptr(30)})
	q1 := mk(domain.Item{Title: "Горит", Important: true, Urgent: true, EstimateMin: ptr(90)})
	q2 := mk(domain.Item{Title: "Важное без оценки", Important: true}) // считается за 30м
	mk(domain.Item{Title: "Жду ответа", Status: domain.StatusWaiting, EstimateMin: ptr(600)})

	p, err := st.Plan(ctx, today)
	if err != nil {
		t.Fatal(err)
	}
	if p.CapacityMin != 180 || p.FreeMin != 120 || p.LoadMin != 150 || p.Unestimated != 1 {
		t.Fatalf("загрузка: cap %d free %d load %d unest %d", p.CapacityMin, p.FreeMin, p.LoadMin, p.Unestimated)
	}
	if len(p.Fits) != 2 || p.Fits[0].ID != q1.ID || p.Fits[1].ID != q2.ID {
		t.Fatalf("влезает: %v", titles(p.Fits))
	}
	if len(p.Overflow) != 1 || p.Overflow[0].ID != q4.ID {
		t.Fatalf("не влезает: %v", titles(p.Overflow))
	}

	// Перенос на завтра: встреча сохраняет время, задача получает перенос.
	tomorrow := today.AddDays(1)
	var moved []domain.Item
	err = st.InTx(ctx, func(tx *store.Store) error {
		var err error
		moved, err = tx.Reschedule(ctx, []string{q4.ID, meet.ID}, &tomorrow, "me")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved[0].PlannedDate.String() != tomorrow.String() || moved[0].PostponeCount != 1 {
		t.Fatalf("задача: %v, переносов %d", moved[0].PlannedDate, moved[0].PostponeCount)
	}
	m := moved[1]
	if got := m.StartAt.In(domain.MSK); got.Format("2006-01-02 15:04") != tomorrow.String()+" 10:00" || m.EndAt.Sub(*m.StartAt) != time.Hour {
		t.Fatalf("встреча: %v – %v", m.StartAt, m.EndAt)
	}
	// В «когда-нибудь» — без даты.
	err = st.InTx(ctx, func(tx *store.Store) error {
		moved, err = tx.Reschedule(ctx, []string{q2.ID}, nil, "me")
		return err
	})
	if err != nil || moved[0].Status != domain.StatusSomeday || moved[0].PlannedDate != nil {
		t.Fatalf("когда-нибудь: %v %+v", err, moved)
	}
}

func titles(items []domain.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

func TestTimer(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	st := store.New(pool)
	a, _ := st.CreateItem(ctx, domain.Item{Title: "А"}, "me")
	b, _ := st.CreateItem(ctx, domain.Item{Title: "Б"}, "me")

	tm, stopped, err := st.StartTimer(ctx, a.ID, "me")
	if err != nil || stopped != nil || tm.Item.ID != a.ID || tm.Item.Status != domain.StatusDoing {
		t.Fatalf("старт: %v %+v %+v", err, tm, stopped)
	}
	// Таймер шёл 25 минут — сдвигаем начало назад.
	if _, err := pool.Exec(ctx, `UPDATE timers SET started_at = now() - interval '25 minutes'`); err != nil {
		t.Fatal(err)
	}
	// Запуск другой задачи останавливает первую и записывает время.
	if _, stopped, err = st.StartTimer(ctx, b.ID, "me"); err != nil || stopped == nil || stopped.Minutes != 25 || stopped.ItemID != a.ID {
		t.Fatalf("переключение: %v %+v", err, stopped)
	}
	cur, _ := st.ActiveTimer(ctx)
	if cur == nil || cur.Item.ID != b.ID {
		t.Fatalf("текущий: %+v", cur)
	}
	e, err := st.StopTimer(ctx, "")
	if err != nil || e == nil || e.Minutes != 1 || e.Source != "timer" {
		t.Fatalf("стоп: %v %+v", err, e)
	}
	if e, _ := st.StopTimer(ctx, ""); e != nil {
		t.Fatal("второй стоп должен быть пустым")
	}
	got, _ := st.GetItem(ctx, a.ID)
	if got.SpentMin != 25 {
		t.Fatalf("факт: %d", got.SpentMin)
	}

	// Долгий таймер — одно напоминание.
	st.StartTimer(ctx, a.ID, "me")
	pool.Exec(ctx, `UPDATE timers SET started_at = now() - interval '4 hours'`)
	if lt, _ := st.LongTimer(ctx, 3*time.Hour); lt == nil {
		t.Fatal("долгий таймер не найден")
	}
	st.MarkTimerReminded(ctx)
	if lt, _ := st.LongTimer(ctx, 3*time.Hour); lt != nil {
		t.Fatal("напомнили дважды")
	}
	if err := st.DiscardTimer(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := st.ActiveTimer(ctx); cur != nil {
		t.Fatal("таймер не сброшен")
	}
}

func TestReminders(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	st := store.New(pool)
	now := time.Now()
	soon := now.Add(10 * time.Minute)
	late := now.Add(2 * time.Hour)
	dlSoon := now.Add(20 * time.Hour)
	dlMissed := now.Add(-3 * time.Hour)
	call, _ := st.CreateItem(ctx, domain.Item{Title: "Созвон", StartAt: &soon}, "me")
	st.CreateItem(ctx, domain.Item{Title: "Потом", StartAt: &late}, "me")
	st.CreateItem(ctx, domain.Item{Title: "Отчёт", Deadline: &dlSoon}, "me")
	st.CreateItem(ctx, domain.Item{Title: "Налоги", Deadline: &dlMissed}, "me")
	mirror, _ := st.CreateItem(ctx, domain.Item{Title: "Чужая встреча", Kind: domain.KindEvent, StartAt: &soon}, "me")
	pool.Exec(ctx, `UPDATE items SET gcal_calendar_id = 'primary', gcal_event_id = 'x' WHERE id = $1`, mirror.ID)

	set := store.DefaultSettings()
	due, err := st.DueReminders(ctx, now, set)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, r := range due {
		kinds[r.Item.Title] = r.Kind
	}
	want := map[string]string{"Созвон": store.RemindStart, "Отчёт": store.RemindDeadlineSoon, "Налоги": store.RemindDeadlineMissed}
	if len(kinds) != len(want) {
		t.Fatalf("напоминания: %v", kinds)
	}
	for k, v := range want {
		if kinds[k] != v {
			t.Fatalf("%s: %q, ждали %q (%v)", k, kinds[k], v, kinds)
		}
	}
	for _, r := range due {
		if ok, err := st.ClaimReminder(ctx, r); !ok || err != nil {
			t.Fatalf("claim: %v %v", ok, err)
		}
	}
	if due, _ := st.DueReminders(ctx, now, set); len(due) != 0 {
		t.Fatalf("повтор: %d", len(due))
	}
	// Перенесли созвон — напомнит снова.
	newStart, _ := json.Marshal(now.Add(5 * time.Minute))
	st.UpdateItem(ctx, call.ID, store.Patch{"start_at": newStart}, "me")
	if due, _ := st.DueReminders(ctx, now, set); len(due) != 1 {
		t.Fatalf("после переноса: %d", len(due))
	}
	// Выключенные напоминания — тишина.
	set.RemindBeforeMin, set.RemindDeadlines = 0, false
	if due, _ := st.DueReminders(ctx, now, set); len(due) != 0 {
		t.Fatalf("выключено: %d", len(due))
	}
}

func TestSettingsAndQuiet(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	s, err := st.Settings(ctx)
	if err != nil || s != store.DefaultSettings() {
		t.Fatalf("по умолчанию: %v %+v", err, s)
	}
	if _, err := st.SaveSettings(ctx, json.RawMessage(`{"capacity_min": 5}`)); err == nil {
		t.Fatal("5 минут в день пропустили")
	}
	if _, err := st.SaveSettings(ctx, json.RawMessage(`{"capasity_min": 300}`)); err == nil {
		t.Fatal("опечатку в поле пропустили")
	}
	if s, err = st.SaveSettings(ctx, json.RawMessage(`{"remind_before_min": 30}`)); err != nil || s.RemindBeforeMin != 30 || s.CapacityMin != 480 {
		t.Fatalf("сохранение: %v %+v", err, s)
	}
	if !s.Quiet(23) || !s.Quiet(3) || s.Quiet(8) || s.Quiet(15) {
		t.Fatal("тихие часы через полночь")
	}
}

func TestHealthMerge(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	d := domain.Today()
	if _, err := st.PutHealth(ctx, store.HealthDay{Date: d}); err == nil {
		t.Fatal("пустой день пропустили")
	}
	if _, err := st.PutHealth(ctx, store.HealthDay{Date: d, SleepMin: ptr(420), BodyBattery: ptr(80), Source: "garmin"}); err != nil {
		t.Fatal(err)
	}
	h, err := st.PutHealth(ctx, store.HealthDay{Date: d, Steps: ptr(9000), Source: "garmin"})
	if err != nil || *h.SleepMin != 420 || *h.Steps != 9000 || *h.BodyBattery != 80 {
		t.Fatalf("дописывание: %v %+v", err, h)
	}
	p, _ := st.Plan(ctx, d)
	if p.Health == nil || *p.Health.SleepMin != 420 {
		t.Fatalf("здоровье в плане дня: %+v", p.Health)
	}
	s, _ := st.Stats(ctx, d, d.AddDays(1))
	if len(s.Health) != 1 {
		t.Fatalf("здоровье в статистике: %+v", s.Health)
	}
}

func TestReviewAndSplit(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	st := store.New(pool)
	d := domain.Today()
	big, _ := st.CreateItem(ctx, domain.Item{Title: "Ремонт", PlannedDate: &d}, "me")
	old, _ := st.CreateItem(ctx, domain.Item{Title: "Старьё"}, "me")
	pool.Exec(ctx, `UPDATE items SET postpone_count = 3 WHERE id = $1`, big.ID)
	pool.Exec(ctx, `UPDATE items SET created_at = now() - interval '30 days' WHERE id = $1`, old.ID)

	r, err := st.Review(ctx)
	if err != nil || len(r.Postponed) != 1 || len(r.Stale) != 1 {
		t.Fatalf("разбор: %v %v %v", err, titles(r.Postponed), titles(r.Stale))
	}
	if n, _ := st.ReviewCount(ctx); n != 1 {
		t.Fatalf("бейдж: %d", n)
	}
	var parent domain.Item
	err = st.InTx(ctx, func(tx *store.Store) error {
		var err error
		parent, err = tx.Split(ctx, big.ID, []string{"Купить краску", " ", "Покрасить"}, "me")
		return err
	})
	if err != nil || parent.PlannedDate != nil {
		t.Fatalf("разбить: %v %+v", err, parent.PlannedDate)
	}
	det, _ := st.Detail(ctx, big.ID)
	if len(det.Children) != 2 || det.Children[0].PlannedDate.String() != d.String() {
		t.Fatalf("подзадачи: %v", titles(det.Children))
	}
	if r, _ := st.Review(ctx); len(r.Postponed) != 0 {
		t.Fatalf("разбитая осталась в разборе: %v", titles(r.Postponed))
	}
}
