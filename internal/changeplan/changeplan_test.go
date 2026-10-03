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

func ops(t *testing.T, s string) []changeplan.Op {
	t.Helper()
	var out []changeplan.Op
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProposeIsDryRunAndApplyCommits(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)

	plan, err := svc.Propose(ctx, "claude", "вечерний разбор", ops(t, `[
		{"op":"create","ref":"call","item":{"kind":"event","title":"Созвон с командой",
			"sphere":"work","start_at":"2026-10-07T16:00:00+03:00","end_at":"2026-10-07T17:00:00+03:00","important":true}},
		{"op":"create","ref":"prep","item":{"title":"Подготовить вопросы","sphere":"work",
			"planned_date":"2026-10-07","estimate_min":30,"tags":["@комп"],
			"body":[{"type":"checklist","items":[{"text":"стек","done":false}]}]}},
		{"op":"relate","from":"$prep","to":"$call","type":"prepares"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Preview) != 3 || !strings.Contains(plan.Preview[0], "ср 07.10 16:00–17:00") {
		t.Fatalf("превью: %q", plan.Preview)
	}
	if items, _ := st.ListItems(ctx, store.Filter{}); len(items) != 0 {
		t.Fatalf("propose не должен писать задачи, есть %d", len(items))
	}

	applied, err := svc.Apply(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.Refs["prep"] == "" {
		t.Fatalf("план: %+v", applied)
	}
	d, err := st.Detail(ctx, applied.Refs["call"])
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Relations) != 1 || d.Relations[0].Type != domain.RelPrepares {
		t.Fatalf("связи: %+v", d.Relations)
	}
	if _, err := svc.Apply(ctx, plan.ID); err == nil {
		t.Fatal("повторное применение должно падать")
	}
}

func TestBrokenPlanAppliesNothing(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)

	_, err := svc.Propose(ctx, "claude", "", ops(t, `[
		{"op":"create","ref":"a","item":{"title":"ок"}},
		{"op":"update","id":"$a","set":{"status":"нет-такого"}}
	]`))
	if err == nil || !strings.Contains(err.Error(), "операция 2") {
		t.Fatalf("ждали ошибку во 2-й операции, got %v", err)
	}
	if items, _ := st.ListItems(ctx, store.Filter{}); len(items) != 0 {
		t.Fatalf("после ошибки остались задачи: %d", len(items))
	}
}

func TestPostponeCountsOnlyOneOff(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)

	p, err := svc.Propose(ctx, "claude", "", ops(t, `[
		{"op":"create","ref":"x","item":{"title":"Разовая","planned_date":"2026-10-04"}},
		{"op":"update","id":"$x","set":{"planned_date":"2026-10-05"}},
		{"op":"update","id":"$x","set":{"planned_date":"2026-10-03"}},
		{"op":"update","id":"$x","set":{"planned_date":"2026-10-06","estimate_min":45}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Preview[1], "перенос №1") {
		t.Fatalf("превью: %q", p.Preview)
	}
	ap, err := svc.Apply(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	it, _ := st.GetItem(ctx, ap.Refs["x"])
	// 04→05 перенос, 05→03 — нет (вперёд), 03→06 перенос.
	if it.PostponeCount != 2 || it.EstimateMin == nil || *it.EstimateMin != 45 {
		t.Fatalf("postpone=%d estimate=%v", it.PostponeCount, it.EstimateMin)
	}
}

func TestDependencyCycleRejected(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	_, err := changeplan.New(st).Propose(ctx, "claude", "", ops(t, `[
		{"op":"create","ref":"a","item":{"title":"A"}},
		{"op":"create","ref":"b","item":{"title":"B"}},
		{"op":"relate","from":"$a","to":"$b","type":"blocks"},
		{"op":"relate","from":"$b","to":"$a","type":"follows"}
	]`))
	if err == nil || !strings.Contains(err.Error(), "цикл") {
		t.Fatalf("ждали цикл, got %v", err)
	}
}

func TestWeightedProgressAndDay(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)
	p, err := svc.Propose(ctx, "claude", "", ops(t, `[
		{"op":"project","ref":"lp","name":"lifeplan","sphere":"product"},
		{"op":"create","ref":"g","item":{"kind":"goal","title":"Запустить v1","project":"$lp"}},
		{"op":"create","ref":"s1","item":{"title":"Схема БД","parent":"$g","weight":4,"planned_date":"2026-10-03","estimate_min":60}},
		{"op":"create","item":{"title":"Бот","parent":"$g","weight":1,"planned_date":"2026-10-03","estimate_min":90}},
		{"op":"done","id":"$s1"},
		{"op":"log_time","id":"$s1","minutes":75}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	ap, err := svc.Apply(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.Detail(ctx, ap.Refs["g"])
	if err != nil {
		t.Fatal(err)
	}
	if d.Progress == nil || *d.Progress != 0.8 {
		t.Fatalf("прогресс: %v", d.Progress)
	}
	date, _ := domain.ParseDate("2026-10-03")
	day, err := st.Day(ctx, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(day.Planned) != 2 || day.PlanMin != 90 || day.Planned[1].SpentMin != 75 {
		t.Fatalf("день: planned=%d planMin=%d", len(day.Planned), day.PlanMin)
	}
}
