package render

import (
	"strings"
	"testing"

	"gitlab.com/KosovAndrey/lifeplan/internal/domain"
	"gitlab.com/KosovAndrey/lifeplan/internal/store"
)

func ptr[T any](v T) *T { return &v }

func TestHealth(t *testing.T) {
	if Health(nil) != "" {
		t.Fatal("нет данных — пустая строка")
	}
	full := &store.HealthDay{SleepMin: ptr(435), SleepScore: ptr(81), BodyBattery: ptr(78), StressAvg: ptr(31), Steps: ptr(8432), RestingHR: ptr(54)}
	if got := Health(full); got != "сон 7ч15 (81) · заряд 78 · стресс 31 · шаги 8432 · пульс 54" {
		t.Fatalf("полный день: %q", got)
	}
	if got := Health(&store.HealthDay{Steps: ptr(900)}); got != "шаги 900" {
		t.Fatalf("только шаги: %q", got)
	}
}

func TestLoad(t *testing.T) {
	if Load(store.DayPlan{}, Opts{}) != "" {
		t.Fatal("пустой день — без строки загрузки")
	}
	p := store.DayPlan{CapacityMin: 480, FreeMin: 420, LoadMin: 510, Unestimated: 2,
		Overflow: []domain.Item{{ID: "abc", Title: "Разобрать почту", Status: domain.StatusTodo}}}
	p.BusyMin = 60
	got := Load(p, Opts{IDs: true})
	for _, want := range []string{"дела 8ч30 при свободных 7ч (день 8ч, встречи 1ч)", "без оценки 2 — по 30м", "не влезает (1):", "Разобрать почту", "#abc"} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет %q в:\n%s", want, got)
		}
	}
}
