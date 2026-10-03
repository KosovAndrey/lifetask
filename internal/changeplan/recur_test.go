package changeplan_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/changeplan"
	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func TestRecurringSeries(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)
	today := domain.Today()

	// Ежедневная тренировка 19:00–20:30 — на горизонт 14 дней это 15 экземпляров (включая сегодня).
	p, err := svc.ApplyNow(ctx, "claude", "", ops(t, `[
		{"op":"recur","rule":"FREQ=DAILY","start":"`+today.String()+`","time":"19:00","duration_min":90,
		 "item":{"title":"Тренировка","sphere":"health","estimate_min":90,"tags":["@вне_дома"]}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Preview[0], "🔁 «Тренировка» каждый день в 19:00") {
		t.Fatalf("превью: %q", p.Preview)
	}
	occ, _ := st.ListItems(ctx, store.Filter{Query: "Тренировка"})
	if len(occ) != store.RecurHorizonDays+1 {
		t.Fatalf("экземпляров: %d", len(occ))
	}
	first := occ[0]
	if first.StartAt == nil || first.StartAt.In(domain.MSK).Format("15:04") != "19:00" ||
		first.EndAt.Sub(*first.StartAt).Minutes() != 90 || len(first.Tags) != 1 {
		t.Fatalf("экземпляр: %+v", first)
	}

	// Перенос экземпляра повтора не считается переносом.
	tomorrow, _ := json.Marshal(today.AddDays(1))
	moved, err := st.UpdateItem(ctx, first.ID, store.Patch{"planned_date": tomorrow}, "me")
	if err != nil {
		t.Fatal(err)
	}
	if moved.PostponeCount != 0 {
		t.Fatalf("перенос повтора посчитан: %d", moved.PostponeCount)
	}

	// Удалённый экземпляр не воскресает при повторной генерации.
	if err := st.DeleteItem(ctx, occ[1].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.GenerateRecurrences(ctx, today.AddDays(store.RecurHorizonDays)); err != nil || n != 0 {
		t.Fatalf("повторная генерация: n=%d err=%v", n, err)
	}
	// Горизонт сдвинулся — появился ровно один новый.
	if n, _ := st.GenerateRecurrences(ctx, today.AddDays(store.RecurHorizonDays+1)); n != 1 {
		t.Fatalf("сдвиг горизонта: n=%d", n)
	}

	// Остановка с послезавтра: сделанный остаётся, будущие несделанные удаляются.
	recs, _ := st.Recurrences(ctx)
	if _, err := st.UpdateItem(ctx, occ[3].ID, store.Patch{"status": json.RawMessage(`"done"`)}, "me"); err != nil {
		t.Fatal(err)
	}
	p, err = svc.ApplyNow(ctx, "claude", "", ops(t, `[{"op":"recur_stop","id":"`+recs[0].ID+`","start":"`+today.AddDays(2).String()+`"}]`))
	if err != nil {
		t.Fatal(err)
	}
	left, _ := st.ListItems(ctx, store.Filter{Query: "Тренировка"})
	// сегодня (перенесён на завтра) + done на день 3; день 1 удалён раньше.
	if len(left) != 2 {
		t.Fatalf("осталось %d: %q", len(left), p.Preview)
	}
	if recs, _ := st.Recurrences(ctx); len(recs) != 0 {
		t.Fatal("остановленная серия в активных")
	}
}

func TestRecurValidation(t *testing.T) {
	ctx := context.Background()
	svc := changeplan.New(store.New(testdb.New(t)))
	for _, bad := range []string{
		`[{"op":"recur","rule":"каждую субботу","item":{"title":"x"}}]`,
		`[{"op":"recur","rule":"FREQ=WEEKLY","item":{"title":"x","planned_date":"2026-10-10"}}]`,
		`[{"op":"recur","rule":"FREQ=WEEKLY","time":"25:00","item":{"title":"x"}}]`,
	} {
		if _, err := svc.Propose(ctx, "claude", "", ops(t, bad)); err == nil {
			t.Errorf("ждали ошибку: %s", bad)
		}
	}
}
