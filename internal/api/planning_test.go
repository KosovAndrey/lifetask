package api_test

import (
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/api"
	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
	"gitlab.com/KosovAndrey/lifeplan/internal/testdb"
)

func TestPlanningRoutes(t *testing.T) {
	c := client{t, api.New(store.New(testdb.New(t)), token).Handler()}
	today := domain.Today().String()
	tomorrow := domain.Today().AddDays(1).String()

	if code := c.do("PATCH", "/api/settings", `{"capacity_min": 60}`, nil); code != 200 {
		t.Fatalf("settings: %d", code)
	}
	var a, b struct{ ID string }
	c.do("POST", "/api/items", `{"title":"Главное","important":true,"estimate_min":60,"planned_date":"`+today+`"}`, &a)
	c.do("POST", "/api/items", `{"title":"Мелочь","estimate_min":15,"planned_date":"`+today+`"}`, &b)

	var plan struct {
		FreeMin  int `json:"free_min"`
		Overflow []struct{ ID string }
	}
	if code := c.do("GET", "/api/plan/today", "", &plan); code != 200 || plan.FreeMin != 60 || len(plan.Overflow) != 1 || plan.Overflow[0].ID != b.ID {
		t.Fatalf("plan: %d %+v", code, plan)
	}
	var moved []struct {
		PlannedDate string `json:"planned_date"`
	}
	if code := c.do("POST", "/api/reschedule", `{"ids":["`+b.ID+`"],"date":"tomorrow"}`, &moved); code != 200 || moved[0].PlannedDate != tomorrow {
		t.Fatalf("reschedule: %d %+v", code, moved)
	}

	var tm struct {
		Timer *struct{ Item struct{ ID string } }
	}
	if code := c.do("POST", "/api/items/"+a.ID+"/timer", "", &tm); code != 200 || tm.Timer.Item.ID != a.ID {
		t.Fatalf("timer start: %d %+v", code, tm)
	}
	var counts struct {
		Inbox, Review int
		Timer         *struct {
			ElapsedMin int `json:"elapsed_min"`
		}
	}
	if code := c.do("GET", "/api/counts", "", &counts); code != 200 || counts.Timer == nil {
		t.Fatalf("counts: %d %+v", code, counts)
	}
	var stop struct{ Entry *struct{ Minutes int } }
	if code := c.do("POST", "/api/timer/stop", "", &stop); code != 200 || stop.Entry == nil || stop.Entry.Minutes != 1 {
		t.Fatalf("timer stop: %d %+v", code, stop)
	}

	var h struct {
		SleepMin int `json:"sleep_min"`
	}
	if code := c.do("PUT", "/api/health/today", `{"sleep_min":440,"source":"garmin"}`, &h); code != 200 || h.SleepMin != 440 {
		t.Fatalf("health: %d %+v", code, h)
	}
	if code := c.do("PUT", "/api/health/today", `{"sleep_min":5000}`, nil); code != 400 {
		t.Fatalf("сон больше суток: %d", code)
	}

	var split struct {
		PlannedDate *string `json:"planned_date"`
	}
	if code := c.do("POST", "/api/items/"+a.ID+"/split", `{"titles":["Шаг 1","Шаг 2"]}`, &split); code != 200 || split.PlannedDate != nil {
		t.Fatalf("split: %d %+v", code, split)
	}
	var review struct{ Postponed, Stale []any }
	if code := c.do("GET", "/api/review", "", &review); code != 200 {
		t.Fatalf("review: %d", code)
	}
}
