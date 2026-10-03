package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func TestPlanRateAndGraph(t *testing.T) {
	ctx := context.Background()
	st := store.New(testdb.New(t))
	y := domain.Today().AddDays(-1)
	mk := func(title string, d domain.Date) domain.Item {
		it, err := st.CreateItem(ctx, domain.Item{Title: title, PlannedDate: &d}, "me")
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	done := mk("сделано", y)
	mk("не сделано", y)
	moved := mk("перенесено", y)
	if _, err := st.UpdateItem(ctx, done.ID, store.Patch{"status": json.RawMessage(`"done"`)}, "me"); err != nil {
		t.Fatal(err)
	}
	next, _ := json.Marshal(domain.Today())
	if _, err := st.UpdateItem(ctx, moved.ID, store.Patch{"planned_date": next}, "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LogTime(ctx, domain.TimeEntry{ItemID: done.ID, Minutes: 30}); err != nil {
		t.Fatal(err)
	}

	s, err := st.Stats(ctx, y, domain.Today().AddDays(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Days) != 2 {
		t.Fatalf("дней: %d", len(s.Days))
	}
	d := s.Days[0]
	if d.PlanDone != 1 || d.PlanMissed != 1 || d.PlanMoved != 1 {
		t.Fatalf("план вчера: %+v", d)
	}
	if s.TotalMinutes != 30 || s.Days[1].Created != 3 {
		t.Fatalf("минуты %d, создано сегодня %d", s.TotalMinutes, s.Days[1].Created)
	}

	// Граф: цель с подзадачей и связь «готовит».
	goal, _ := st.CreateItem(ctx, domain.Item{Kind: domain.KindGoal, Title: "Цель"}, "me")
	child, _ := st.CreateItem(ctx, domain.Item{Title: "Часть", ParentID: &goal.ID}, "me")
	if err := st.Relate(ctx, domain.Relation{FromID: done.ID, ToID: child.ID, Type: domain.RelPrepares}); err != nil {
		t.Fatal(err)
	}
	g, err := st.Graph(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 3 || len(g.Edges) != 2 {
		t.Fatalf("граф: %d узлов, %d рёбер", len(g.Nodes), len(g.Edges))
	}
	det, _ := st.Detail(ctx, child.ID)
	if len(det.Related) != 1 || det.Related[0].Title != "сделано" {
		t.Fatalf("связанные: %+v", det.Related)
	}
}
