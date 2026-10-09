package changeplan_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

func TestApplyRejectsStaleItemAndPreservesChanges(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)
	it, err := st.CreateItem(ctx, domain.Item{Title: "original"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Propose(ctx, "claude", "", []changeplan.Op{
		{Op: "create", Item: map[string]json.RawMessage{"title": json.RawMessage(`"must not survive"`)}},
		{Op: "update", ID: it.ID, Set: map[string]json.RawMessage{"title": json.RawMessage(`"proposed"`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateItem(ctx, it.ID, store.Patch{"title": json.RawMessage(`"newer edit"`)}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, p.ID); !errors.Is(err, changeplan.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	items, err := st.ListItems(ctx, store.Filter{})
	if err != nil || len(items) != 1 || items[0].Title != "newer edit" {
		t.Fatalf("items after conflict: %+v, %v", items, err)
	}
	p, err = svc.Get(ctx, p.ID)
	if err != nil || p.Status != "proposed" {
		t.Fatalf("plan after conflict: %+v, %v", p, err)
	}
}

func TestApplyRejectsDeletedReference(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	it, err := st.CreateItem(ctx, domain.Item{Title: "gone"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	svc := changeplan.New(st)
	p, err := svc.Propose(ctx, "claude", "", []changeplan.Op{{Op: "done", ID: it.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteItem(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, p.ID); !errors.Is(err, changeplan.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func TestConcurrentApplyCreatesExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := testdb.New(t)
	st := store.New(pool)
	svc := changeplan.New(st)
	p, err := svc.Propose(ctx, "claude", "", ops(t, `[{"op":"create","item":{"title":"only once"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if _, err := gate.Exec(ctx, `SELECT 1 FROM change_plans WHERE id=$1 FOR UPDATE`, p.ID); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := svc.Apply(ctx, p.ID)
			results <- err
		}()
	}
	waitForPlanLock(t, ctx, st, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var applied, conflicts int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			applied++
		case errors.Is(err, changeplan.ErrConflict):
			conflicts++
		default:
			t.Fatalf("apply: %v", err)
		}
	}
	if applied != 1 || conflicts != 1 {
		t.Fatalf("applied=%d conflicts=%d", applied, conflicts)
	}
	items, err := st.ListItems(ctx, store.Filter{})
	if err != nil || len(items) != 1 {
		t.Fatalf("items: %+v, %v", items, err)
	}
}

func TestApplyWaitsForConcurrentRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := testdb.New(t)
	st := store.New(pool)
	svc := changeplan.New(st)
	p, err := svc.Propose(ctx, "claude", "", ops(t, `[{"op":"create","item":{"title":"rejected"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	reject, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reject.Rollback(context.Background())
	if _, err := reject.Exec(ctx, `UPDATE change_plans SET status='rejected' WHERE id=$1 AND status='proposed'`, p.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := svc.Apply(ctx, p.ID)
		result <- err
	}()
	waitForPlanLock(t, ctx, st, 1)
	if err := reject.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, changeplan.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	items, err := st.ListItems(ctx, store.Filter{})
	if err != nil || len(items) != 0 {
		t.Fatalf("rejected plan created items: %+v, %v", items, err)
	}
	p, err = svc.Get(ctx, p.ID)
	if err != nil || p.Status != "rejected" {
		t.Fatalf("plan: %+v, %v", p, err)
	}
}

func waitForPlanLock(t *testing.T, ctx context.Context, st *store.Store, count int) {
	t.Helper()
	for {
		var waiting int
		err := st.Raw().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query LIKE '%change_plans%'`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting >= count {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestApplyNowInsideTransactionDoesNotKeepDryRunWrites(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	err := st.InTx(ctx, func(tx *store.Store) error {
		_, err := changeplan.New(tx).ApplyNow(ctx, "claude", "", ops(t, `[
			{"op":"create","ref":"a","item":{"title":"once"}},
			{"op":"log_time","id":"$a","minutes":15}
		]`))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListItems(ctx, store.Filter{})
	if err != nil || len(items) != 1 || items[0].SpentMin != 15 {
		t.Fatalf("dry-run writes survived: %+v, %v", items, err)
	}
}

func TestApplyRollsBackEarlierOperations(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	svc := changeplan.New(st)
	a, err := st.CreateItem(ctx, domain.Item{Title: "A"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateItem(ctx, domain.Item{Title: "B"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Propose(ctx, "claude", "", []changeplan.Op{
		{Op: "create", Item: map[string]json.RawMessage{"title": json.RawMessage(`"rolled back"`)}},
		{Op: "relate", From: a.ID, To: b.ID, Type: "blocks"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Relations can change independently of item rows. The second operation now
	// creates a dependency cycle and must undo the successful first operation.
	if err := st.Relate(ctx, domain.Relation{FromID: b.ID, ToID: a.ID, Type: domain.RelBlocks}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, p.ID); err == nil || !strings.Contains(err.Error(), "операция 2") {
		t.Fatalf("want failure in second operation, got %v", err)
	}
	items, err := st.ListItems(ctx, store.Filter{})
	if err != nil || len(items) != 2 {
		t.Fatalf("partial apply survived: %+v, %v", items, err)
	}
	p, err = svc.Get(ctx, p.ID)
	if err != nil || p.Status != "proposed" || p.AppliedAt != nil {
		t.Fatalf("failed plan marked applied: %+v, %v", p, err)
	}
}

func TestProposedItemCanBeChangedSeveralTimesInOnePlan(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	it, err := st.CreateItem(ctx, domain.Item{Title: "original"}, "me")
	if err != nil {
		t.Fatal(err)
	}
	svc := changeplan.New(st)
	p, err := svc.Propose(ctx, "claude", "", []changeplan.Op{
		{Op: "update", ID: it.ID, Set: map[string]json.RawMessage{"title": json.RawMessage(`"renamed"`)}},
		{Op: "done", ID: it.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A Google sync acknowledgement must not invalidate the user's preview.
	if _, err := st.Raw().Exec(ctx, `UPDATE items SET gcal_calendar_id='cal', gcal_event_id='event',
		sync_etag='etag', gcal_synced_at=updated_at WHERE id=$1`, it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Apply(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	it, err = st.GetItem(ctx, it.ID)
	if err != nil || it.Title != "renamed" || it.Status != domain.StatusDone {
		t.Fatalf("item: %+v, %v", it, err)
	}
}

func TestApplyRejectsChangedParentAndRelationTargets(t *testing.T) {
	for _, kind := range []string{"parent", "parent_id", "relation", "tags"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			st := store.New(testdb.New(t))
			it, err := st.CreateItem(ctx, domain.Item{Title: "referenced"}, "me")
			if err != nil {
				t.Fatal(err)
			}
			planOps := []changeplan.Op{{Op: "create", Ref: "new", Item: map[string]json.RawMessage{"title": json.RawMessage(`"new"`)}}}
			switch kind {
			case "parent", "parent_id":
				id, _ := json.Marshal(it.ID)
				planOps[0].Item[kind] = id
			case "relation":
				planOps = append(planOps, changeplan.Op{Op: "relate", From: "$new", To: it.ID, Type: "prepares"})
			case "tags":
				planOps = []changeplan.Op{{Op: "done", ID: it.ID}}
			}
			svc := changeplan.New(st)
			p, err := svc.Propose(ctx, "claude", "", planOps)
			if err != nil {
				t.Fatal(err)
			}
			patch := store.Patch{"title": json.RawMessage(`"changed"`)}
			if kind == "tags" {
				patch = store.Patch{"tags": json.RawMessage(`["edited"]`)}
			}
			if _, err := st.UpdateItem(ctx, it.ID, patch, "me"); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Apply(ctx, p.ID); !errors.Is(err, changeplan.ErrConflict) {
				t.Fatalf("want conflict for changed %s reference, got %v", kind, err)
			}
		})
	}
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
